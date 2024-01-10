package proc

import (
	"bytes"
	"debug/dwarf"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/dwarf/op"
	"github.com/go-delve/delve/pkg/dwarf/reader"
	"github.com/go-delve/delve/pkg/goversion"
	"github.com/go-delve/delve/pkg/logflags"
	"github.com/go-delve/delve/pkg/proc/evalop"
)

var errOperationOnSpecialFloat = errors.New("operations on non-finite floats not implemented")

const goDictionaryName = ".dict"

// EvalScope is the scope for variable evaluation. Contains the thread,
// current location (PC), and canonical frame address.
type EvalScope struct {
	Location
	Regs     op.DwarfRegisters
	Mem      MemoryReadWriter // Target's memory
	g        *G
	threadID int
	BinInfo  *BinaryInfo
	target   *Target
	loadCfg  *LoadConfig

	frameOffset int64

	// When the following pointer is not nil this EvalScope was created
	// by EvalExpressionWithCalls and function call injection are allowed.
	// See the top comment in fncall.go for a description of how the call
	// injection protocol is handled.
	callCtx *callContext

	dictAddr uint64 // dictionary address for instantiated generic functions
}

type localsFlags uint8

const (
	// If localsTrustArgOrder is set function arguments that don't have an
	// address will have one assigned by looking at their position in the argument
	// list.
	localsTrustArgOrder localsFlags = 1 << iota

	// If localsNoDeclLineCheck the declaration line isn't checked at
	// all to determine if the variable is in scope.
	localsNoDeclLineCheck
)

// ConvertEvalScope returns a new EvalScope in the context of the
// specified goroutine ID and stack frame.
// If deferCall is > 0 the eval scope will be relative to the specified deferred call.
func ConvertEvalScope(dbp *Target, gid int64, frame, deferCall int) (*EvalScope, error) {
	if _, err := dbp.Valid(); err != nil {
		return nil, err
	}
	ct := dbp.CurrentThread()
	threadID := ct.ThreadID()
	g, err := FindGoroutine(dbp, gid)
	if err != nil {
		return nil, err
	}

	var opts StacktraceOptions
	if deferCall > 0 {
		opts = StacktraceReadDefers
	}

	var locs []Stackframe
	if g != nil {
		if g.Thread != nil {
			threadID = g.Thread.ThreadID()
		}
		locs, err = GoroutineStacktrace(dbp, g, frame+1, opts)
	} else {
		locs, err = ThreadStacktrace(dbp, ct, frame+1)
	}
	if err != nil {
		return nil, err
	}

	if frame >= len(locs) {
		return nil, fmt.Errorf("Frame %d does not exist in goroutine %d", frame, gid)
	}

	if deferCall > 0 {
		if deferCall-1 >= len(locs[frame].Defers) {
			return nil, fmt.Errorf("Frame %d only has %d deferred calls", frame, len(locs[frame].Defers))
		}

		d := locs[frame].Defers[deferCall-1]
		if d.Unreadable != nil {
			return nil, d.Unreadable
		}

		return d.EvalScope(dbp, ct)
	}

	return FrameToScope(dbp, dbp.Memory(), g, threadID, locs[frame:]...), nil
}

// FrameToScope returns a new EvalScope for frames[0].
// If frames has at least two elements all memory between
// frames[0].Regs.SP() and frames[1].Regs.CFA will be cached.
// Otherwise all memory between frames[0].Regs.SP() and frames[0].Regs.CFA
// will be cached.
func FrameToScope(t *Target, thread MemoryReadWriter, g *G, threadID int, frames ...Stackframe) *EvalScope {
	// Creates a cacheMem that will preload the entire stack frame the first
	// time any local variable is read.
	// Remember that the stack grows downward in memory.
	minaddr := frames[0].Regs.SP()
	var maxaddr uint64
	if len(frames) > 1 && frames[0].SystemStack == frames[1].SystemStack {
		maxaddr = uint64(frames[1].Regs.CFA)
	} else {
		maxaddr = uint64(frames[0].Regs.CFA)
	}
	if maxaddr > minaddr && maxaddr-minaddr < maxFramePrefetchSize {
		thread = cacheMemory(thread, minaddr, int(maxaddr-minaddr))
	}

	s := &EvalScope{Location: frames[0].Call, Regs: frames[0].Regs, Mem: thread, g: g, BinInfo: t.BinInfo(), target: t, frameOffset: frames[0].FrameOffset(), threadID: threadID}
	s.PC = frames[0].lastpc
	return s
}

// ThreadScope returns an EvalScope for the given thread.
func ThreadScope(t *Target, thread Thread) (*EvalScope, error) {
	locations, err := ThreadStacktrace(t, thread, 1)
	if err != nil {
		return nil, err
	}
	if len(locations) < 1 {
		return nil, errors.New("could not decode first frame")
	}
	return FrameToScope(t, thread.ProcessMemory(), nil, thread.ThreadID(), locations...), nil
}

// GoroutineScope returns an EvalScope for the goroutine running on the given thread.
func GoroutineScope(t *Target, thread Thread) (*EvalScope, error) {
	locations, err := ThreadStacktrace(t, thread, 1)
	if err != nil {
		return nil, err
	}
	if len(locations) < 1 {
		return nil, errors.New("could not decode first frame")
	}
	g, err := GetG(thread)
	if err != nil {
		return nil, err
	}
	threadID := 0
	if g.Thread != nil {
		threadID = g.Thread.ThreadID()
	}
	return FrameToScope(t, thread.ProcessMemory(), g, threadID, locations...), nil
}

// EvalExpression returns the value of the given expression.
func (scope *EvalScope) EvalExpression(expr string, cfg LoadConfig) (*Variable, error) {
	ops, err := evalop.Compile(scopeToEvalLookup{scope}, expr, false)
	if err != nil {
		return nil, err
	}

	stack := &evalStack{}

	scope.loadCfg = &cfg
	stack.eval(scope, ops)
	ev, err := stack.result(&cfg)
	if err != nil {
		return nil, err
	}

	ev.loadValue(cfg)
	if ev.Name == "" {
		ev.Name = expr
	}
	return ev, nil
}

type scopeToEvalLookup struct {
	*EvalScope
}

func (s scopeToEvalLookup) FindTypeExpr(expr ast.Expr) (godwarf.Type, error) {
	return s.BinInfo.findTypeExpr(expr)
}

func (scope scopeToEvalLookup) HasLocal(name string) bool {
	if scope.Fn == nil {
		return false
	}

	flags := reader.VariablesOnlyVisible
	if scope.BinInfo.Producer() != "" && goversion.ProducerAfterOrEqual(scope.BinInfo.Producer(), 1, 15) {
		flags |= reader.VariablesTrustDeclLine
	}

	dwarfTree, err := scope.image().getDwarfTree(scope.Fn.offset)
	if err != nil {
		return false
	}

	varEntries := reader.Variables(dwarfTree, scope.PC, scope.Line, flags)
	for _, entry := range varEntries {
		curname, _ := entry.Val(dwarf.AttrName).(string)
		if curname == name {
			return true
		}
		if len(curname) > 0 && curname[0] == '&' {
			if curname[1:] == name {
				return true
			}
		}
	}
	return false
}

func (scope scopeToEvalLookup) HasGlobal(pkgName, varName string) bool {
	hasGlobalInternal := func(name string) bool {
		for _, pkgvar := range scope.BinInfo.packageVars {
			if pkgvar.name == name || strings.HasSuffix(pkgvar.name, "/"+name) {
				return true
			}
		}
		for _, fn := range scope.BinInfo.Functions {
			if fn.Name == name || strings.HasSuffix(fn.Name, "/"+name) {
				return true
			}
		}
		for _, ctyp := range scope.BinInfo.consts {
			for _, cval := range ctyp.values {
				if cval.fullName == name || strings.HasSuffix(cval.fullName, "/"+name) {
					return true
				}
			}
		}
		return false
	}

	if pkgName == "" {
		if scope.Fn == nil {
			return false
		}
		return hasGlobalInternal(scope.Fn.PackageName() + "." + varName)
	}

	for _, pkgPath := range scope.BinInfo.PackageMap[pkgName] {
		if hasGlobalInternal(pkgPath + "." + varName) {
			return true
		}
	}
	return hasGlobalInternal(pkgName + "." + varName)
}

func (scope scopeToEvalLookup) LookupRegisterName(name string) (int, bool) {
	s := validRegisterName(name)
	if s == "" {
		return 0, false
	}
	return scope.BinInfo.Arch.RegisterNameToDwarf(s)
}

func (scope scopeToEvalLookup) HasBuiltin(name string) bool {
	return supportedBuiltins[name] != nil
}

// ChanGoroutines returns the list of goroutines waiting to receive from or
// send to the channel.
func (scope *EvalScope) ChanGoroutines(expr string, start, count int) ([]int64, error) {
	t, err := parser.ParseExpr(expr)
	if err != nil {
		return nil, err
	}
	v, err := scope.evalAST(t)
	if err != nil {
		return nil, err
	}
	if v.Kind != reflect.Chan {
		return nil, nil
	}

	structMemberMulti := func(v *Variable, names ...string) *Variable {
		for _, name := range names {
			var err error
			v, err = v.structMember(name)
			if err != nil {
				return nil
			}
		}
		return v
	}

	waitqFirst := func(qname string) *Variable {
		qvar := structMemberMulti(v, qname, "first")
		if qvar == nil {
			return nil
		}
		return qvar.maybeDereference()
	}

	var goids []int64

	waitqToGoIDSlice := func(qvar *Variable) error {
		if qvar == nil {
			return nil
		}
		for {
			if qvar.Addr == 0 {
				return nil
			}
			if len(goids) > count {
				return nil
			}
			goidVar := structMemberMulti(qvar, "g", "goid")
			if goidVar == nil {
				return nil
			}
			goidVar.loadValue(loadSingleValue)
			if goidVar.Unreadable != nil {
				return goidVar.Unreadable
			}
			goid, _ := constant.Int64Val(goidVar.Value)
			if start > 0 {
				start--
			} else {
				goids = append(goids, goid)
			}

			nextVar, err := qvar.structMember("next")
			if err != nil {
				return err
			}
			qvar = nextVar.maybeDereference()
		}
	}

	recvqVar := waitqFirst("recvq")
	err = waitqToGoIDSlice(recvqVar)
	if err != nil {
		return nil, err
	}
	sendqVar := waitqFirst("sendq")
	err = waitqToGoIDSlice(sendqVar)
	if err != nil {
		return nil, err
	}
	return goids, nil
}

// Locals returns all variables in 'scope'.
func (scope *EvalScope) Locals(flags localsFlags) ([]*Variable, error) {
	if scope.Fn == nil {
		return nil, errors.New("unable to find function context")
	}

	trustArgOrder := (flags&localsTrustArgOrder != 0) && scope.BinInfo.Producer() != "" && goversion.ProducerAfterOrEqual(scope.BinInfo.Producer(), 1, 12) && scope.Fn != nil && (scope.PC == scope.Fn.Entry)

	dwarfTree, err := scope.image().getDwarfTree(scope.Fn.offset)
	if err != nil {
		return nil, err
	}

	variablesFlags := reader.VariablesOnlyVisible
	if flags&localsNoDeclLineCheck != 0 {
		variablesFlags = reader.VariablesNoDeclLineCheck
	}
	if scope.BinInfo.Producer() != "" && goversion.ProducerAfterOrEqual(scope.BinInfo.Producer(), 1, 15) {
		variablesFlags |= reader.VariablesTrustDeclLine
	}

	varEntries := reader.Variables(dwarfTree, scope.PC, scope.Line, variablesFlags)

	// look for dictionary entry
	if scope.dictAddr == 0 {
		for _, entry := range varEntries {
			name, _ := entry.Val(dwarf.AttrName).(string)
			if name == goDictionaryName {
				dictVar, err := extractVarInfoFromEntry(scope.target, scope.BinInfo, scope.image(), scope.Regs, scope.Mem, entry.Tree, 0)
				if err != nil {
					logflags.DebuggerLogger().Errorf("could not load %s variable: %v", name, err)
				} else if dictVar.Unreadable != nil {
					logflags.DebuggerLogger().Errorf("could not load %s variable: %v", name, dictVar.Unreadable)
				} else {
					scope.dictAddr, err = readUintRaw(dictVar.mem, dictVar.Addr, int64(scope.BinInfo.Arch.PtrSize()))
					if err != nil {
						logflags.DebuggerLogger().Errorf("could not load %s variable: %v", name, err)
					}
				}
				break
			}
		}
	}

	vars := make([]*Variable, 0, len(varEntries))
	depths := make([]int, 0, len(varEntries))
	for _, entry := range varEntries {
		if name, _ := entry.Val(dwarf.AttrName).(string); name == goDictionaryName {
			continue
		}
		val, err := extractVarInfoFromEntry(scope.target, scope.BinInfo, scope.image(), scope.Regs, scope.Mem, entry.Tree, scope.dictAddr)
		if err != nil {
			// skip variables that we can't parse yet
			continue
		}
		if trustArgOrder && ((val.Unreadable != nil && val.Addr == 0) || val.Flags&VariableFakeAddress != 0) && entry.Tag == dwarf.TagFormalParameter {
			addr := afterLastArgAddr(vars)
			if addr == 0 {
				addr = uint64(scope.Regs.CFA)
			}
			addr = uint64(alignAddr(int64(addr), val.DwarfType.Align()))
			val = newVariable(val.Name, addr, val.DwarfType, scope.BinInfo, scope.Mem)
		}
		vars = append(vars, val)
		depth := entry.Depth
		if entry.Tag == dwarf.TagFormalParameter {
			if depth <= 1 {
				depth = 0
			}
			isret, _ := entry.Val(dwarf.AttrVarParam).(bool)
			if isret {
				val.Flags |= VariableReturnArgument
			} else {
				val.Flags |= VariableArgument
			}
		}
		depths = append(depths, depth)
	}

	if len(vars) == 0 {
		return vars, nil
	}

	sort.Stable(&variablesByDepthAndDeclLine{vars, depths})

	lvn := map[string]*Variable{} // lvn[n] is the last variable we saw named n

	for i, v := range vars {
		if name := v.Name; len(name) > 1 && name[0] == '&' {
			locationExpr := v.LocationExpr
			declLine := v.DeclLine
			v = v.maybeDereference()
			if v.Addr == 0 && v.Unreadable == nil {
				v.Unreadable = fmt.Errorf("no address for escaped variable")
			}
			v.Name = name[1:]
			v.Flags |= VariableEscaped
			// See https://github.com/go-delve/delve/issues/2049 for details
			if locationExpr != nil {
				locationExpr.isEscaped = true
				v.LocationExpr = locationExpr
			}
			v.DeclLine = declLine
			vars[i] = v
		}
		if otherv := lvn[v.Name]; otherv != nil {
			otherv.Flags |= VariableShadowed
		}
		lvn[v.Name] = v
	}

	return vars, nil
}

func afterLastArgAddr(vars []*Variable) uint64 {
	for i := len(vars) - 1; i >= 0; i-- {
		v := vars[i]
		if (v.Flags&VariableArgument != 0) || (v.Flags&VariableReturnArgument != 0) {
			return v.Addr + uint64(v.DwarfType.Size())
		}
	}
	return 0
}

// setValue writes the value of srcv to dstv.
//   - If srcv is a numerical literal constant and srcv is of a compatible type
//     the necessary type conversion is performed.
//   - If srcv is nil and dstv is of a nil'able type then dstv is nilled.
//   - If srcv is the empty string and dstv is a string then dstv is set to the
//     empty string.
//   - If dstv is an "interface {}" and srcv is either an interface (possibly
//     non-empty) or a pointer shaped type (map, channel, pointer or struct
//     containing a single pointer field) the type conversion to "interface {}"
//     is performed.
//   - If srcv and dstv have the same type and are both addressable then the
//     contents of srcv are copied byte-by-byte into dstv
func (scope *EvalScope) setValue(dstv, srcv *Variable, srcExpr string) error {
	srcv.loadValue(loadSingleValue)

	typerr := srcv.isType(dstv.RealType, dstv.Kind)
	if _, isTypeConvErr := typerr.(*typeConvErr); isTypeConvErr {
		// attempt iface -> eface and ptr-shaped -> eface conversions.
		return convertToEface(srcv, dstv)
	}
	if typerr != nil {
		return typerr
	}

	if srcv.Unreadable != nil {
		//lint:ignore ST1005 backwards compatibility
		return fmt.Errorf("Expression %q is unreadable: %v", srcExpr, srcv.Unreadable)
	}

	// Numerical types
	switch dstv.Kind {
	case reflect.Float32, reflect.Float64:
		f, _ := constant.Float64Val(srcv.Value)
		return dstv.writeFloatRaw(f, dstv.RealType.Size())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, _ := constant.Int64Val(srcv.Value)
		return dstv.writeUint(uint64(n), dstv.RealType.Size())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, _ := constant.Uint64Val(srcv.Value)
		return dstv.writeUint(n, dstv.RealType.Size())
	case reflect.Bool:
		return dstv.writeBool(constant.BoolVal(srcv.Value))
	case reflect.Complex64, reflect.Complex128:
		real, _ := constant.Float64Val(constant.Real(srcv.Value))
		imag, _ := constant.Float64Val(constant.Imag(srcv.Value))
		return dstv.writeComplex(real, imag, dstv.RealType.Size())
	case reflect.Func:
		if dstv.RealType.Size() == 0 {
			if dstv.Name != "" {
				return fmt.Errorf("can not assign to %s", dstv.Name)
			}
			return errors.New("can not assign to function expression")
		}
	}

	// nilling nillable variables
	if srcv == nilVariable {
		return dstv.writeZero()
	}

	if srcv.Kind == reflect.String {
		if srcv.Base == 0 && srcv.Len > 0 && srcv.Flags&VariableConstant != 0 {
			return errFuncCallNotAllowedStrAlloc
		}
		return dstv.writeString(uint64(srcv.Len), srcv.Base)
	}

	// slice assignment (this is not handled by the writeCopy below so that
	// results of a reslice operation can be used here).
	if srcv.Kind == reflect.Slice {
		return dstv.writeSlice(srcv.Len, srcv.Cap, srcv.Base)
	}

	// allow any integer to be converted to any pointer
	if t, isptr := dstv.RealType.(*godwarf.PtrType); isptr {
		return dstv.writeUint(srcv.Children[0].Addr, t.ByteSize)
	}

	// byte-by-byte copying for everything else, but the source must be addressable
	if srcv.Addr != 0 {
		return dstv.writeCopy(srcv)
	}

	return fmt.Errorf("can not set variables of type %s (not implemented)", dstv.Kind.String())
}

// SetVariable sets the value of the named variable
func (scope *EvalScope) SetVariable(name, value string) error {
	ops, err := evalop.CompileSet(scopeToEvalLookup{scope}, name, value)
	if err != nil {
		return err
	}

	stack := &evalStack{}
	stack.eval(scope, ops)
	_, err = stack.result(nil)
	return err
}

// LocalVariables returns all local variables from the current function scope.
func (scope *EvalScope) LocalVariables(cfg LoadConfig) ([]*Variable, error) {
	vars, err := scope.Locals(0)
	if err != nil {
		return nil, err
	}
	vars = filterVariables(vars, func(v *Variable) bool {
		return (v.Flags & (VariableArgument | VariableReturnArgument)) == 0
	})
	cfg.MaxMapBuckets = maxMapBucketsFactor * cfg.MaxArrayValues
	loadValues(vars, cfg)
	return vars, nil
}

// FunctionArguments returns the name, value, and type of all current function arguments.
func (scope *EvalScope) FunctionArguments(cfg LoadConfig) ([]*Variable, error) {
	vars, err := scope.Locals(0)
	if err != nil {
		return nil, err
	}
	vars = filterVariables(vars, func(v *Variable) bool {
		return (v.Flags & (VariableArgument | VariableReturnArgument)) != 0
	})
	cfg.MaxMapBuckets = maxMapBucketsFactor * cfg.MaxArrayValues
	loadValues(vars, cfg)
	return vars, nil
}

func filterVariables(vars []*Variable, pred func(v *Variable) bool) []*Variable {
	r := make([]*Variable, 0, len(vars))
	for i := range vars {
		if pred(vars[i]) {
			r = append(r, vars[i])
		}
	}
	return r
}

func regsReplaceStaticBase(regs op.DwarfRegisters, image *Image) op.DwarfRegisters {
	regs.StaticBase = image.StaticBase
	return regs
}

// PackageVariables returns the name, value, and type of all package variables in the application.
func (scope *EvalScope) PackageVariables(cfg LoadConfig) ([]*Variable, error) {
	pkgvars := make([]packageVar, len(scope.BinInfo.packageVars))
	copy(pkgvars, scope.BinInfo.packageVars)
	sort.Slice(pkgvars, func(i, j int) bool {
		if pkgvars[i].cu.image.addr == pkgvars[j].cu.image.addr {
			return pkgvars[i].offset < pkgvars[j].offset
		}
		return pkgvars[i].cu.image.addr < pkgvars[j].cu.image.addr
	})
	vars := make([]*Variable, 0, len(scope.BinInfo.packageVars))
	for _, pkgvar := range pkgvars {
		reader := pkgvar.cu.image.dwarfReader
		reader.Seek(pkgvar.offset)
		entry, err := reader.Next()
		if err != nil {
			return nil, err
		}

		// Ignore errors trying to extract values
		val, err := extractVarInfoFromEntry(scope.target, scope.BinInfo, pkgvar.cu.image, regsReplaceStaticBase(scope.Regs, pkgvar.cu.image), scope.Mem, godwarf.EntryToTree(entry), 0)
		if val != nil && val.Kind == reflect.Invalid {
			continue
		}
		if err != nil {
			continue
		}
		val.loadValue(cfg)
		vars = append(vars, val)
	}

	return vars, nil
}

func (scope *EvalScope) findGlobal(pkgName, varName string) (*Variable, error) {
	for _, pkgPath := range scope.BinInfo.PackageMap[pkgName] {
		v, err := scope.findGlobalInternal(pkgPath + "." + varName)
		if err != nil || v != nil {
			return v, err
		}
	}
	v, err := scope.findGlobalInternal(pkgName + "." + varName)
	if err != nil || v != nil {
		return v, err
	}
	return nil, fmt.Errorf("could not find symbol value for %s.%s", pkgName, varName)
}

func (scope *EvalScope) findGlobalInternal(name string) (*Variable, error) {
	for _, pkgvar := range scope.BinInfo.packageVars {
		if pkgvar.name == name || strings.HasSuffix(pkgvar.name, "/"+name) {
			reader := pkgvar.cu.image.dwarfReader
			reader.Seek(pkgvar.offset)
			entry, err := reader.Next()
			if err != nil {
				return nil, err
			}
			return extractVarInfoFromEntry(scope.target, scope.BinInfo, pkgvar.cu.image, regsReplaceStaticBase(scope.Regs, pkgvar.cu.image), scope.Mem, godwarf.EntryToTree(entry), 0)
		}
	}
	for _, fn := range scope.BinInfo.Functions {
		if fn.Name == name || strings.HasSuffix(fn.Name, "/"+name) {
			//TODO(aarzilli): convert function entry into a function type?
			r := newVariable(fn.Name, fn.Entry, &godwarf.FuncType{}, scope.BinInfo, scope.Mem)
			r.Value = constant.MakeString(fn.Name)
			r.Base = fn.Entry
			r.loaded = true
			if fn.Entry == 0 {
				r.Unreadable = fmt.Errorf("function %s is inlined", fn.Name)
			}
			return r, nil
		}
	}
	for dwref, ctyp := range scope.BinInfo.consts {
		for _, cval := range ctyp.values {
			if cval.fullName == name || strings.HasSuffix(cval.fullName, "/"+name) {
				t, err := scope.BinInfo.Images[dwref.imageIndex].Type(dwref.offset)
				if err != nil {
					return nil, err
				}
				v := newVariable(name, 0x0, t, scope.BinInfo, scope.Mem)
				switch v.Kind {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					v.Value = constant.MakeInt64(cval.value)
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
					v.Value = constant.MakeUint64(uint64(cval.value))
				default:
					return nil, fmt.Errorf("unsupported constant kind %v", v.Kind)
				}
				v.Flags |= VariableConstant
				v.loaded = true
				return v, nil
			}
		}
	}
	return nil, nil
}

// image returns the image containing the current function.
func (scope *EvalScope) image() *Image {
	return scope.BinInfo.funcToImage(scope.Fn)
}

// evalStack stores the stack machine used to evaluate a program made of
// evalop.Ops.
// When an opcode sets callInjectionContinue execution of the program will be suspended
// and the call injection protocol will be executed instead.
type evalStack struct {
	stack                 []*Variable          // current stack of Variable values
	fncalls               []*functionCallState // stack of call injections currently being executed
	ops                   []evalop.Op          // program being executed
	opidx                 int                  // program counter for the stack program
	callInjectionContinue bool                 // when set program execution suspends and the call injection protocol is executed instead
	err                   error

	spoff, bpoff, fboff int64
	scope               *EvalScope
	curthread           Thread
	lastRetiredFncall   *functionCallState
}

func (s *evalStack) push(v *Variable) {
	s.stack = append(s.stack, v)
}

func (s *evalStack) pop() *Variable {
	v := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	return v
}

func (s *evalStack) peek() *Variable {
	return s.stack[len(s.stack)-1]
}

func (s *evalStack) fncallPush(fncall *functionCallState) {
	s.fncalls = append(s.fncalls, fncall)
}

func (s *evalStack) fncallPop() *functionCallState {
	fncall := s.fncalls[len(s.fncalls)-1]
	s.fncalls = s.fncalls[:len(s.fncalls)-1]
	return fncall
}

func (s *evalStack) fncallPeek() *functionCallState {
	return s.fncalls[len(s.fncalls)-1]
}

func (s *evalStack) pushErr(v *Variable, err error) {
	s.err = err
	s.stack = append(s.stack, v)
}

// eval evaluates ops. When it returns if callInjectionContinue is set the
// target program should be resumed to execute the call injection protocol.
// Otherwise the result of the evaluation can be retrieved using
// stack.result.
func (stack *evalStack) eval(scope *EvalScope, ops []evalop.Op) {
	if logflags.FnCall() {
		fncallLog("eval program:\n%s", evalop.Listing(nil, ops))
	}

	stack.ops = ops
	stack.scope = scope

	if scope.g != nil {
		stack.spoff = int64(scope.Regs.Uint64Val(scope.Regs.SPRegNum)) - int64(scope.g.stack.hi)
		stack.bpoff = int64(scope.Regs.Uint64Val(scope.Regs.BPRegNum)) - int64(scope.g.stack.hi)
		stack.fboff = scope.Regs.FrameBase - int64(scope.g.stack.hi)
	}

	if scope.g != nil && scope.g.Thread != nil {
		stack.curthread = scope.g.Thread
	}

	stack.run()
}

// resume resumes evaluation of stack.ops. When it returns if
// callInjectionContinue is set the target program should be resumed to
// execute the call injection protocol. Otherwise the result of the
// evaluation can be retrieved using stack.result.
func (stack *evalStack) resume(g *G) {
	stack.callInjectionContinue = false
	scope := stack.scope
	// Go 1.15 will move call injection execution to a different goroutine,
	// but we want to keep evaluation on the original goroutine.
	if g.ID == scope.g.ID {
		scope.g = g
	} else {
		// We are in Go 1.15 and we switched to a new goroutine, the original
		// goroutine is now parked and therefore does not have a thread
		// associated.
		scope.g.Thread = nil
		scope.g.Status = Gwaiting
		scope.callCtx.injectionThread = g.Thread
	}

	// adjust the value of registers inside scope
	pcreg, bpreg, spreg := scope.Regs.Reg(scope.Regs.PCRegNum), scope.Regs.Reg(scope.Regs.BPRegNum), scope.Regs.Reg(scope.Regs.SPRegNum)
	scope.Regs.ClearRegisters()
	scope.Regs.AddReg(scope.Regs.PCRegNum, pcreg)
	scope.Regs.AddReg(scope.Regs.BPRegNum, bpreg)
	scope.Regs.AddReg(scope.Regs.SPRegNum, spreg)
	scope.Regs.Reg(scope.Regs.SPRegNum).Uint64Val = uint64(stack.spoff + int64(scope.g.stack.hi))
	scope.Regs.Reg(scope.Regs.BPRegNum).Uint64Val = uint64(stack.bpoff + int64(scope.g.stack.hi))
	scope.Regs.FrameBase = stack.fboff + int64(scope.g.stack.hi)
	scope.Regs.CFA = scope.frameOffset + int64(scope.g.stack.hi)
	stack.curthread = g.Thread

	finished := funcCallStep(scope, stack, g.Thread)
	if finished {
		funcCallFinish(scope, stack)
	}

	if stack.callInjectionContinue {
		// not done with call injection, stay in this mode
		stack.scope.callCtx.injectionThread = nil
		return
	}

	// call injection protocol suspended or concluded, resume normal opcode execution
	stack.run()
}

func (stack *evalStack) run() {
	scope, curthread := stack.scope, stack.curthread
	for stack.opidx < len(stack.ops) && stack.err == nil {
		stack.callInjectionContinue = false
		stack.executeOp()
		// If the instruction we just executed requests the call injection
		// protocol by setting callInjectionContinue we switch to it.
		if stack.callInjectionContinue {
			scope.callCtx.injectionThread = nil
			return
		}
	}

	if stack.err == nil && len(stack.fncalls) > 0 {
		stack.err = fmt.Errorf("internal debugger error: eval program finished without error but %d call injections still active", len(stack.fncalls))
		return
	}

	// If there is an error we must undo all currently executing call
	// injections before returning.

	if len(stack.fncalls) > 0 {
		fncall := stack.fncallPeek()
		if fncall == stack.lastRetiredFncall {
			stack.err = fmt.Errorf("internal debugger error: could not undo injected call during error recovery, original error: %v", stack.err)
			return
		}
		if fncall.undoInjection != nil {
			// setTargetExecuted is set if evalop.CallInjectionSetTarget has been
			// executed but evalop.CallInjectionComplete hasn't, we must undo the callOP
			// call in evalop.CallInjectionSetTarget before continuing.
			switch scope.BinInfo.Arch.Name {
			case "amd64":
				regs, _ := curthread.Registers()
				setSP(curthread, regs.SP()+uint64(scope.BinInfo.Arch.PtrSize()))
				setPC(curthread, fncall.undoInjection.oldpc)
			case "arm64", "ppc64le":
				setLR(curthread, fncall.undoInjection.oldlr)
				setPC(curthread, fncall.undoInjection.oldpc)
			default:
				panic("not implemented")
			}
		}
		stack.lastRetiredFncall = fncall
		// Resume target to undo one call
		stack.callInjectionContinue = true
		scope.callCtx.injectionThread = nil
		return
	}
}

func (stack *evalStack) result(cfg *LoadConfig) (*Variable, error) {
	var r *Variable
	switch len(stack.stack) {
	case 0:
		// ok
	case 1:
		r = stack.peek()
	default:
		if stack.err == nil {
			stack.err = fmt.Errorf("internal debugger error: wrong stack size at end %d", len(stack.stack))
		}
	}
	if r != nil && cfg != nil && stack.err == nil {
		r.loadValue(*cfg)
	}
	return r, stack.err
}

// executeOp executes the opcode at stack.ops[stack.opidx] and increments stack.opidx.
func (stack *evalStack) executeOp() {
	scope, ops, curthread := stack.scope, stack.ops, stack.curthread
	defer func() {
		err := recover()
		if err != nil {
			stack.err = fmt.Errorf("internal debugger error: %v (recovered)\n%s", err, string(debug.Stack()))
		}
	}()
	switch op := ops[stack.opidx].(type) {
	case *evalop.PushCurg:
		if scope.g != nil {
			stack.push(scope.g.variable.clone())
		} else {
			typ, err := scope.BinInfo.findType("runtime.g")
			if err != nil {
				stack.err = fmt.Errorf("could not find runtime.g: %v", err)
				return
			}
			gvar := newVariable("curg", fakeAddressUnresolv, typ, scope.BinInfo, scope.Mem)
			gvar.loaded = true
			gvar.Flags = VariableFakeAddress
			gvar.Children = append(gvar.Children, *newConstant(constant.MakeInt64(0), scope.Mem))
			gvar.Children[0].Name = "goid"
			stack.push(gvar)
		}

	case *evalop.PushFrameoff:
		stack.push(newConstant(constant.MakeInt64(scope.frameOffset), scope.Mem))

	case *evalop.PushThreadID:
		stack.push(newConstant(constant.MakeInt64(int64(scope.threadID)), scope.Mem))

	case *evalop.PushConst:
		stack.push(newConstant(op.Value, scope.Mem))

	case *evalop.PushLocal:
		var vars []*Variable
		var err error
		if op.Frame != 0 {
			frameScope, err2 := ConvertEvalScope(scope.target, -1, int(op.Frame), 0)
			if err2 != nil {
				stack.err = err2
				return
			}
			vars, err = frameScope.Locals(0)
		} else {
			vars, err = scope.Locals(0)
		}
		if err != nil {
			stack.err = err
			return
		}
		found := false
		for i := range vars {
			if vars[i].Name == op.Name && vars[i].Flags&VariableShadowed == 0 {
				stack.push(vars[i])
				found = true
				break
			}
		}
		if !found {
			stack.err = fmt.Errorf("could not find symbol value for %s", op.Name)
		}

	case *evalop.PushNil:
		stack.push(nilVariable)

	case *evalop.PushRegister:
		reg := scope.Regs.Reg(uint64(op.Regnum))
		if reg == nil {
			stack.err = fmt.Errorf("could not find symbol value for %s", op.Regname)
			return
		}
		reg.FillBytes()

		var typ godwarf.Type
		if len(reg.Bytes) <= 8 {
			typ = godwarf.FakeBasicType("uint", 64)
		} else {
			var err error
			typ, err = scope.BinInfo.findType("string")
			if err != nil {
				stack.err = err
				return
			}
		}

		v := newVariable(op.Regname, 0, typ, scope.BinInfo, scope.Mem)
		if v.Kind == reflect.String {
			v.Len = int64(len(reg.Bytes) * 2)
			v.Base = fakeAddressUnresolv
		}
		v.Addr = fakeAddressUnresolv
		v.Flags = VariableCPURegister
		v.reg = reg
		stack.push(v)

	case *evalop.PushPackageVar:
		pkgName := op.PkgName
		replaceName := false
		if pkgName == "" {
			replaceName = true
			pkgName = scope.Fn.PackageName()
		}
		v, err := scope.findGlobal(pkgName, op.Name)
		if err != nil {
			stack.err = err
			return
		}
		if replaceName {
			v.Name = op.Name
		}
		stack.push(v)

	case *evalop.Select:
		scope.evalStructSelector(op, stack)

	case *evalop.TypeAssert:
		scope.evalTypeAssert(op, stack)

	case *evalop.PointerDeref:
		scope.evalPointerDeref(op, stack)

	case *evalop.Unary:
		scope.evalUnary(op, stack)

	case *evalop.AddrOf:
		scope.evalAddrOf(op, stack)

	case *evalop.TypeCast:
		scope.evalTypeCast(op, stack)

	case *evalop.Reslice:
		scope.evalReslice(op, stack)

	case *evalop.Index:
		scope.evalIndex(op, stack)

	case *evalop.Jump:
		scope.evalJump(op, stack)

	case *evalop.Binary:
		scope.evalBinary(op, stack)

	case *evalop.BoolToConst:
		x := stack.pop()
		if x.Kind != reflect.Bool {
			stack.err = errors.New("internal debugger error: expected boolean")
			return
		}
		x.loadValue(loadFullValue)
		stack.push(newConstant(x.Value, scope.Mem))

	case *evalop.Pop:
		stack.pop()

	case *evalop.BuiltinCall:
		vars := make([]*Variable, len(op.Args))
		for i := len(op.Args) - 1; i >= 0; i-- {
			vars[i] = stack.pop()
		}
		stack.pushErr(supportedBuiltins[op.Name](vars, op.Args))

	case *evalop.CallInjectionStart:
		scope.evalCallInjectionStart(op, stack)

	case *evalop.CallInjectionSetTarget:
		scope.evalCallInjectionSetTarget(op, stack, curthread)

	case *evalop.CallInjectionCopyArg:
		fncall := stack.fncallPeek()
		actualArg := stack.pop()
		if actualArg.Name == "" {
			actualArg.Name = exprToString(op.ArgExpr)
		}
		stack.err = funcCallCopyOneArg(scope, fncall, actualArg, &fncall.formalArgs[op.ArgNum], curthread)

	case *evalop.CallInjectionComplete:
		stack.fncallPeek().undoInjection = nil
		stack.callInjectionContinue = true

	case *evalop.CallInjectionAllocString:
		stack.callInjectionContinue = scope.allocString(op.Phase, stack, curthread)

	case *evalop.SetValue:
		lhv := stack.pop()
		rhv := stack.pop()
		stack.err = scope.setValue(lhv, rhv, exprToString(op.Rhe))

	default:
		stack.err = fmt.Errorf("internal debugger error: unknown eval opcode: %#v", op)
	}

	stack.opidx++
}

func (scope *EvalScope) evalAST(t ast.Expr) (*Variable, error) {
	ops, err := evalop.CompileAST(scopeToEvalLookup{scope}, t)
	if err != nil {
		return nil, err
	}
	stack := &evalStack{}
	stack.eval(scope, ops)
	return stack.result(nil)
}

func exprToString(t ast.Expr) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, token.NewFileSet(), t)
	return buf.String()
}

func (scope *EvalScope) evalJump(op *evalop.Jump, stack *evalStack) {
	x := stack.peek()
	if op.Pop {
		stack.pop()
	}

	var v bool
	switch op.When {
	case evalop.JumpIfTrue:
		v = true
	case evalop.JumpIfFalse:
		v = false
	}

	if x.Kind != reflect.Bool {
		if op.Node != nil {
			stack.err = fmt.Errorf("expression %q should be boolean not %s", exprToString(op.Node), x.Kind)
		} else {
			stack.err = errors.New("internal debugger error: expected boolean")
		}
		return
	}
	x.loadValue(loadFullValue)
	if x.Unreadable != nil {
		stack.err = x.Unreadable
		return
	}
	if constant.BoolVal(x.Value) == v {
		stack.opidx = op.Target - 1
	}
}

// Eval type cast expressions
func (scope *EvalScope) evalTypeCast(op *evalop.TypeCast, stack *evalStack) {
	argv := stack.pop()

	typ := resolveTypedef(op.DwarfType)

	converr := fmt.Errorf("can not convert %q to %s", exprToString(op.Node.Args[0]), typ.String())

	// compatible underlying types
	if typeCastCompatibleTypes(argv.RealType, typ) {
		if ptyp, isptr := typ.(*godwarf.PtrType); argv.Kind == reflect.Ptr && argv.loaded && len(argv.Children) > 0 && isptr {
			cv := argv.Children[0]
			argv.Children[0] = *newVariable(cv.Name, cv.Addr, ptyp.Type, cv.bi, cv.mem)
			argv.Children[0].OnlyAddr = true
		}
		argv.RealType = typ
		argv.DwarfType = op.DwarfType
		stack.push(argv)
		return
	}

	v := newVariable("", 0, op.DwarfType, scope.BinInfo, scope.Mem)
	v.loaded = true

	switch ttyp := typ.(type) {
	case *godwarf.PtrType:
		switch argv.Kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			// ok
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			// ok
		default:
			stack.err = converr
			return
		}

		argv.loadValue(loadSingleValue)
		if argv.Unreadable != nil {
			stack.err = argv.Unreadable
			return
		}

		n, _ := constant.Int64Val(argv.Value)

		mem := scope.Mem
		if scope.target != nil {
			if mem2 := scope.target.findFakeMemory(uint64(n)); mem2 != nil {
				mem = mem2
			}
		}

		v.Children = []Variable{*(newVariable("", uint64(n), ttyp.Type, scope.BinInfo, mem))}
		v.Children[0].OnlyAddr = true
		stack.push(v)
		return

	case *godwarf.UintType:
		argv.loadValue(loadSingleValue)
		if argv.Unreadable != nil {
			stack.err = argv.Unreadable
			return
		}
		switch argv.Kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, _ := constant.Int64Val(argv.Value)
			v.Value = constant.MakeUint64(convertInt(uint64(n), false, ttyp.Size()))
			stack.push(v)
			return
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, _ := constant.Uint64Val(argv.Value)
			v.Value = constant.MakeUint64(convertInt(n, false, ttyp.Size()))
			stack.push(v)
			return
		case reflect.Float32, reflect.Float64:
			x, _ := constant.Float64Val(argv.Value)
			v.Value = constant.MakeUint64(uint64(x))
			stack.push(v)
			return
		case reflect.Ptr:
			v.Value = constant.MakeUint64(argv.Children[0].Addr)
			stack.push(v)
			return
		}
	case *godwarf.IntType:
		argv.loadValue(loadSingleValue)
		if argv.Unreadable != nil {
			stack.err = argv.Unreadable
			return
		}
		switch argv.Kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, _ := constant.Int64Val(argv.Value)
			v.Value = constant.MakeInt64(int64(convertInt(uint64(n), true, ttyp.Size())))
			stack.push(v)
			return
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, _ := constant.Uint64Val(argv.Value)
			v.Value = constant.MakeInt64(int64(convertInt(n, true, ttyp.Size())))
			stack.push(v)
			return
		case reflect.Float32, reflect.Float64:
			x, _ := constant.Float64Val(argv.Value)
			v.Value = constant.MakeInt64(int64(x))
			stack.push(v)
			return
		}
	case *godwarf.FloatType:
		argv.loadValue(loadSingleValue)
		if argv.Unreadable != nil {
			stack.err = argv.Unreadable
			return
		}
		switch argv.Kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fallthrough
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			fallthrough
		case reflect.Float32, reflect.Float64:
			v.Value = argv.Value
			stack.push(v)
			return
		}
	case *godwarf.ComplexType:
		argv.loadValue(loadSingleValue)
		if argv.Unreadable != nil {
			stack.err = argv.Unreadable
			return
		}
		switch argv.Kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fallthrough
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			fallthrough
		case reflect.Float32, reflect.Float64:
			v.Value = argv.Value
			stack.push(v)
			return
		}
	}

	cfg := loadFullValue
	if scope.loadCfg != nil {
		cfg = *scope.loadCfg
	}

	switch ttyp := typ.(type) {
	case *godwarf.SliceType:
		switch ttyp.ElemType.Common().ReflectKind {
		case reflect.Uint8:
			// string -> []uint8
			if argv.Kind != reflect.String {
				stack.err = converr
				return
			}
			cfg.MaxStringLen = cfg.MaxArrayValues
			argv.loadValue(cfg)
			if argv.Unreadable != nil {
				stack.err = argv.Unreadable
				return
			}
			for i, ch := range []byte(constant.StringVal(argv.Value)) {
				e := newVariable("", argv.Addr+uint64(i), typ.(*godwarf.SliceType).ElemType, scope.BinInfo, argv.mem)
				e.loaded = true
				e.Value = constant.MakeInt64(int64(ch))
				v.Children = append(v.Children, *e)
			}
			v.Len = argv.Len
			v.Cap = v.Len
			stack.push(v)
			return

		case reflect.Int32:
			// string -> []rune
			if argv.Kind != reflect.String {
				stack.err = converr
				return
			}
			argv.loadValue(cfg)
			if argv.Unreadable != nil {
				stack.err = argv.Unreadable
				return
			}
			for i, ch := range constant.StringVal(argv.Value) {
				e := newVariable("", argv.Addr+uint64(i), typ.(*godwarf.SliceType).ElemType, scope.BinInfo, argv.mem)
				e.loaded = true
				e.Value = constant.MakeInt64(int64(ch))
				v.Children = append(v.Children, *e)
			}
			v.Len = int64(len(v.Children))
			v.Cap = v.Len
			stack.push(v)
			return
		}

	case *godwarf.StringType:
		switch argv.Kind {
		case reflect.String:
			// string -> string
			argv.DwarfType = v.DwarfType
			argv.RealType = v.RealType
			stack.push(argv)
			return
		case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint, reflect.Uintptr:
			// integer -> string
			argv.loadValue(cfg)
			if argv.Unreadable != nil {
				stack.err = argv.Unreadable
				return
			}
			b, _ := constant.Int64Val(argv.Value)
			s := string(rune(b))
			v.Value = constant.MakeString(s)
			v.Len = int64(len(s))
			stack.push(v)
			return
		case reflect.Slice, reflect.Array:
			var elem godwarf.Type
			if argv.Kind == reflect.Slice {
				elem = argv.RealType.(*godwarf.SliceType).ElemType
			} else {
				elem = argv.RealType.(*godwarf.ArrayType).Type
			}
			switch elemType := elem.(type) {
			case *godwarf.UintType:
				// []uint8 -> string
				if elemType.Name != "uint8" && elemType.Name != "byte" {
					stack.err = converr
					return
				}
				cfg.MaxArrayValues = cfg.MaxStringLen
				argv.loadValue(cfg)
				if argv.Unreadable != nil {
					stack.err = argv.Unreadable
					return
				}
				bytes := make([]byte, len(argv.Children))
				for i := range argv.Children {
					n, _ := constant.Int64Val(argv.Children[i].Value)
					bytes[i] = byte(n)
				}
				v.Value = constant.MakeString(string(bytes))
				v.Len = argv.Len

			case *godwarf.IntType:
				// []rune -> string
				if elemType.Name != "int32" && elemType.Name != "rune" {
					stack.err = converr
					return
				}
				cfg.MaxArrayValues = cfg.MaxStringLen
				argv.loadValue(cfg)
				if argv.Unreadable != nil {
					stack.err = argv.Unreadable
					return
				}
				runes := make([]rune, len(argv.Children))
				for i := range argv.Children {
					n, _ := constant.Int64Val(argv.Children[i].Value)
					runes[i] = rune(n)
				}
				v.Value = constant.MakeString(string(runes))
				// The following line is wrong but the only way to get the correct value
				// would be to decode the entire slice.
				v.Len = int64(len(constant.StringVal(v.Value)))

			default:
				stack.err = converr
				return
			}
			stack.push(v)
			return
		}
	}

	stack.err = converr
}

// typeCastCompatibleTypes returns true if typ1 and typ2 are compatible for
// a type cast where only the type of the variable is changed.
func typeCastCompatibleTypes(typ1, typ2 godwarf.Type) bool {
	if typ1 == nil || typ2 == nil || typ1.Common().Size() != typ2.Common().Size() || typ1.Common().Align() != typ2.Common().Align() {
		return false
	}

	if typ1.String() == typ2.String() {
		return true
	}

	switch ttyp1 := typ1.(type) {
	case *godwarf.PtrType:
		if ttyp2, ok := typ2.(*godwarf.PtrType); ok {
			_, isvoid1 := ttyp1.Type.(*godwarf.VoidType)
			_, isvoid2 := ttyp2.Type.(*godwarf.VoidType)
			if isvoid1 || isvoid2 {
				return true
			}
			// pointer types are compatible if their element types are compatible
			return typeCastCompatibleTypes(resolveTypedef(ttyp1.Type), resolveTypedef(ttyp2.Type))
		}
	case *godwarf.StringType:
		if _, ok := typ2.(*godwarf.StringType); ok {
			return true
		}
	case *godwarf.StructType:
		if ttyp2, ok := typ2.(*godwarf.StructType); ok {
			// struct types are compatible if they have the same fields
			if len(ttyp1.Field) != len(ttyp2.Field) {
				return false
			}
			for i := range ttyp1.Field {
				if *ttyp1.Field[i] != *ttyp2.Field[i] {
					return false
				}
			}
			return true
		}
	case *godwarf.ComplexType:
		if _, ok := typ2.(*godwarf.ComplexType); ok {
			// size and alignment already checked above
			return true
		}
	case *godwarf.FloatType:
		if _, ok := typ2.(*godwarf.FloatType); ok {
			// size and alignment already checked above
			return true
		}
	case *godwarf.IntType:
		if _, ok := typ2.(*godwarf.IntType); ok {
			// size and alignment already checked above
			return true
		}
	case *godwarf.UintType:
		if _, ok := typ2.(*godwarf.UintType); ok {
			// size and alignment already checked above
			return true
		}
	case *godwarf.BoolType:
		if _, ok := typ2.(*godwarf.BoolType); ok {
			// size and alignment already checked above
			return true
		}
	}

	return false
}

func convertInt(n uint64, signed bool, size int64) uint64 {
	bits := uint64(size) * 8
	mask := uint64((1 << bits) - 1)
	r := n & mask
	if signed && (r>>(bits-1)) != 0 {
		// sign extension
		r |= ^uint64(0) &^ mask
	}
	return r
}

var supportedBuiltins = map[string]func([]*Variable, []ast.Expr) (*Variable, error){
	"cap":     capBuiltin,
	"len":     lenBuiltin,
	"complex": complexBuiltin,
	"imag":    imagBuiltin,
	"real":    realBuiltin,
	"min":     minBuiltin,
	"max":     maxBuiltin,
}

func capBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments to cap: %d", len(args))
	}

	arg := args[0]
	invalidArgErr := fmt.Errorf("invalid argument %s (type %s) for cap", exprToString(nodeargs[0]), arg.TypeString())

	switch arg.Kind {
	case reflect.Ptr:
		arg = arg.maybeDereference()
		if arg.Kind != reflect.Array {
			return nil, invalidArgErr
		}
		fallthrough
	case reflect.Array:
		return newConstant(constant.MakeInt64(arg.Len), arg.mem), nil
	case reflect.Slice:
		return newConstant(constant.MakeInt64(arg.Cap), arg.mem), nil
	case reflect.Chan:
		arg.loadValue(loadFullValue)
		if arg.Unreadable != nil {
			return nil, arg.Unreadable
		}
		if arg.Base == 0 {
			return newConstant(constant.MakeInt64(0), arg.mem), nil
		}
		return newConstant(arg.Children[1].Value, arg.mem), nil
	default:
		return nil, invalidArgErr
	}
}

func lenBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments to len: %d", len(args))
	}
	arg := args[0]
	invalidArgErr := fmt.Errorf("invalid argument %s (type %s) for len", exprToString(nodeargs[0]), arg.TypeString())

	switch arg.Kind {
	case reflect.Ptr:
		arg = arg.maybeDereference()
		if arg.Kind != reflect.Array {
			return nil, invalidArgErr
		}
		fallthrough
	case reflect.Array, reflect.Slice, reflect.String:
		if arg.Unreadable != nil {
			return nil, arg.Unreadable
		}
		return newConstant(constant.MakeInt64(arg.Len), arg.mem), nil
	case reflect.Chan:
		arg.loadValue(loadFullValue)
		if arg.Unreadable != nil {
			return nil, arg.Unreadable
		}
		if arg.Base == 0 {
			return newConstant(constant.MakeInt64(0), arg.mem), nil
		}
		return newConstant(arg.Children[0].Value, arg.mem), nil
	case reflect.Map:
		it := arg.mapIterator()
		if arg.Unreadable != nil {
			return nil, arg.Unreadable
		}
		if it == nil {
			return newConstant(constant.MakeInt64(0), arg.mem), nil
		}
		return newConstant(constant.MakeInt64(arg.Len), arg.mem), nil
	default:
		return nil, invalidArgErr
	}
}

func complexBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("wrong number of arguments to complex: %d", len(args))
	}

	realev := args[0]
	imagev := args[1]

	realev.loadValue(loadSingleValue)
	imagev.loadValue(loadSingleValue)

	if realev.Unreadable != nil {
		return nil, realev.Unreadable
	}

	if imagev.Unreadable != nil {
		return nil, imagev.Unreadable
	}

	if realev.Value == nil || ((realev.Value.Kind() != constant.Int) && (realev.Value.Kind() != constant.Float)) {
		return nil, fmt.Errorf("invalid argument 1 %s (type %s) to complex", exprToString(nodeargs[0]), realev.TypeString())
	}

	if imagev.Value == nil || ((imagev.Value.Kind() != constant.Int) && (imagev.Value.Kind() != constant.Float)) {
		return nil, fmt.Errorf("invalid argument 2 %s (type %s) to complex", exprToString(nodeargs[1]), imagev.TypeString())
	}

	sz := int64(0)
	if realev.RealType != nil {
		sz = realev.RealType.(*godwarf.FloatType).Size()
	}
	if imagev.RealType != nil {
		isz := imagev.RealType.(*godwarf.FloatType).Size()
		if isz > sz {
			sz = isz
		}
	}

	if sz == 0 {
		sz = 128
	}

	typ := godwarf.FakeBasicType("complex", int(sz))

	r := realev.newVariable("", 0, typ, nil)
	r.Value = constant.BinaryOp(realev.Value, token.ADD, constant.MakeImag(imagev.Value))
	return r, nil
}

func imagBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments to imag: %d", len(args))
	}

	arg := args[0]
	arg.loadValue(loadSingleValue)

	if arg.Unreadable != nil {
		return nil, arg.Unreadable
	}

	if arg.Kind != reflect.Complex64 && arg.Kind != reflect.Complex128 {
		return nil, fmt.Errorf("invalid argument %s (type %s) to imag", exprToString(nodeargs[0]), arg.TypeString())
	}

	return newConstant(constant.Imag(arg.Value), arg.mem), nil
}

func realBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments to real: %d", len(args))
	}

	arg := args[0]
	arg.loadValue(loadSingleValue)

	if arg.Unreadable != nil {
		return nil, arg.Unreadable
	}

	if arg.Value == nil || ((arg.Value.Kind() != constant.Int) && (arg.Value.Kind() != constant.Float) && (arg.Value.Kind() != constant.Complex)) {
		return nil, fmt.Errorf("invalid argument %s (type %s) to real", exprToString(nodeargs[0]), arg.TypeString())
	}

	return newConstant(constant.Real(arg.Value), arg.mem), nil
}

func minBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	return minmaxBuiltin("min", token.LSS, args, nodeargs)
}

func maxBuiltin(args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	return minmaxBuiltin("max", token.GTR, args, nodeargs)
}

func minmaxBuiltin(name string, op token.Token, args []*Variable, nodeargs []ast.Expr) (*Variable, error) {
	var best *Variable

	for i := range args {
		if args[i].Kind == reflect.String {
			args[i].loadValue(loadFullValueLongerStrings)
		} else {
			args[i].loadValue(loadFullValue)
		}

		if args[i].Unreadable != nil {
			return nil, fmt.Errorf("could not load %q: %v", exprToString(nodeargs[i]), args[i].Unreadable)
		}
		if args[i].FloatSpecial != 0 {
			return nil, errOperationOnSpecialFloat
		}

		if best == nil {
			best = args[i]
			continue
		}

		_, err := negotiateType(op, args[i], best)
		if err != nil {
			return nil, err
		}

		v, err := compareOp(op, args[i], best)
		if err != nil {
			return nil, err
		}

		if v {
			best = args[i]
		}
	}

	if best == nil {
		return nil, fmt.Errorf("not enough arguments to %s", name)
	}
	return best, nil
}

// Evaluates expressions <subexpr>.<field name> where subexpr is not a package name
func (scope *EvalScope) evalStructSelector(op *evalop.Select, stack *evalStack) {
	xv := stack.pop()
	// Prevent abuse, attempting to call "nil.member" directly.
	if xv.Addr == 0 && xv.Name == "nil" {
		stack.err = fmt.Errorf("%s (type %s) is not a struct", xv.Name, xv.TypeString())
		return
	}
	// Prevent abuse, attempting to call "\"fake\".member" directly.
	if xv.Addr == 0 && xv.Name == "" && xv.DwarfType == nil && xv.RealType == nil {
		stack.err = fmt.Errorf("%s (type %s) is not a struct", xv.Value, xv.TypeString())
		return
	}
	// Special type conversions for CPU register variables (REGNAME.int8, etc)
	if xv.Flags&VariableCPURegister != 0 && !xv.loaded {
		stack.pushErr(xv.registerVariableTypeConv(op.Name))
		return
	}

	rv, err := xv.findMethod(op.Name)
	if err != nil {
		stack.err = err
		return
	}
	if rv != nil {
		stack.push(rv)
		return
	}
	stack.pushErr(xv.structMember(op.Name))
}

// Evaluates expressions <subexpr>.(<type>)
func (scope *EvalScope) evalTypeAssert(op *evalop.TypeAssert, stack *evalStack) {
	xv := stack.pop()
	if xv.Kind != reflect.Interface {
		stack.err = fmt.Errorf("expression %q not an interface", exprToString(op.Node.X))
		return
	}
	xv.loadInterface(0, false, loadFullValue)
	if xv.Unreadable != nil {
		stack.err = xv.Unreadable
		return
	}
	if xv.Children[0].Unreadable != nil {
		stack.err = xv.Children[0].Unreadable
		return
	}
	if xv.Children[0].Addr == 0 {
		stack.err = fmt.Errorf("interface conversion: %s is nil, not %s", xv.DwarfType.String(), exprToString(op.Node.Type))
		return
	}
	typ := op.DwarfType
	if typ != nil && xv.Children[0].DwarfType.Common().Name != typ.Common().Name {
		stack.err = fmt.Errorf("interface conversion: %s is %s, not %s", xv.DwarfType.Common().Name, xv.Children[0].TypeString(), typ.Common().Name)
		return
	}
	// loadInterface will set OnlyAddr for the data member since here we are
	// passing false to loadData, however returning the variable with OnlyAddr
	// set here would be wrong since, once the expression evaluation
	// terminates, the value of this variable will be loaded.
	xv.Children[0].OnlyAddr = false
	stack.push(&xv.Children[0])
}

// Evaluates expressions <subexpr>[<subexpr>] (subscript access to arrays, slices and maps)
func (scope *EvalScope) evalIndex(op *evalop.Index, stack *evalStack) {
	idxev := stack.pop()
	xev := stack.pop()
	if xev.Unreadable != nil {
		stack.err = xev.Unreadable
		return
	}

	if xev.Flags&VariableCPtr == 0 {
		xev = xev.maybeDereference()
	}

	cantindex := fmt.Errorf("expression %q (%s) does not support indexing", exprToString(op.Node.X), xev.TypeString())

	switch xev.Kind {
	case reflect.Ptr:
		if xev == nilVariable {
			stack.err = cantindex
			return
		}
		if xev.Flags&VariableCPtr == 0 {
			_, isarrptr := xev.RealType.(*godwarf.PtrType).Type.(*godwarf.ArrayType)
			if !isarrptr {
				stack.err = cantindex
				return
			}
			xev = xev.maybeDereference()
		}
		fallthrough

	case reflect.Slice, reflect.Array, reflect.String:
		if xev.Base == 0 {
			stack.err = fmt.Errorf("can not index %q", exprToString(op.Node.X))
			return
		}
		n, err := idxev.asInt()
		if err != nil {
			stack.err = err
			return
		}
		stack.pushErr(xev.sliceAccess(int(n)))
		return

	case reflect.Map:
		idxev.loadValue(loadFullValue)
		if idxev.Unreadable != nil {
			stack.err = idxev.Unreadable
			return
		}
		stack.pushErr(xev.mapAccess(idxev))
		return
	default:
		stack.err = cantindex
		return
	}
}

// Evaluates expressions <subexpr>[<subexpr>:<subexpr>]
// HACK: slicing a map expression with [0:0] will return the whole map
func (scope *EvalScope) evalReslice(op *evalop.Reslice, stack *evalStack) {
	low, err := stack.pop().asInt()
	if err != nil {
		stack.err = err
		return
	}
	var high int64
	if op.HasHigh {
		high, err = stack.pop().asInt()
		if err != nil {
			stack.err = err
			return
		}
	}
	xev := stack.pop()
	if xev.Unreadable != nil {
		stack.err = xev.Unreadable
		return
	}
	if !op.HasHigh {
		high = xev.Len
	}

	switch xev.Kind {
	case reflect.Slice, reflect.Array, reflect.String:
		if xev.Base == 0 {
			stack.err = fmt.Errorf("can not slice %q", exprToString(op.Node.X))
			return
		}
		stack.pushErr(xev.reslice(low, high))
		return
	case reflect.Map:
		if op.Node.High != nil {
			stack.err = fmt.Errorf("second slice argument must be empty for maps")
			return
		}
		xev.mapSkip += int(low)
		xev.mapIterator() // reads map length
		if int64(xev.mapSkip) >= xev.Len {
			stack.err = fmt.Errorf("map index out of bounds")
			return
		}
		stack.push(xev)
		return
	case reflect.Ptr:
		if xev.Flags&VariableCPtr != 0 {
			stack.pushErr(xev.reslice(low, high))
			return
		}
		fallthrough
	default:
		stack.err = fmt.Errorf("can not slice %q (type %s)", exprToString(op.Node.X), xev.TypeString())
		return
	}
}

// Evaluates a pointer dereference expression: *<subexpr>
func (scope *EvalScope) evalPointerDeref(op *evalop.PointerDeref, stack *evalStack) {
	xev := stack.pop()

	if xev.Kind != reflect.Ptr {
		stack.err = fmt.Errorf("expression %q (%s) can not be dereferenced", exprToString(op.Node.X), xev.TypeString())
		return
	}

	if xev == nilVariable {
		stack.err = fmt.Errorf("nil can not be dereferenced")
		return
	}

	if len(xev.Children) == 1 {
		// this branch is here to support pointers constructed with typecasts from ints
		xev.Children[0].OnlyAddr = false
		stack.push(&(xev.Children[0]))
		return
	}
	xev.loadPtr()
	if xev.Unreadable != nil {
		val, ok := constant.Uint64Val(xev.Value)
		if ok && val == 0 {
			stack.err = fmt.Errorf("couldn't read pointer: %w", xev.Unreadable)
			return
		}
	}
	rv := &xev.Children[0]
	if rv.Addr == 0 {
		stack.err = fmt.Errorf("nil pointer dereference")
		return
	}
	stack.push(rv)
}

// Evaluates expressions &<subexpr>
func (scope *EvalScope) evalAddrOf(op *evalop.AddrOf, stack *evalStack) {
	xev := stack.pop()
	if xev.Addr == 0 || xev.DwarfType == nil {
		stack.err = fmt.Errorf("can not take address of %q", exprToString(op.Node.X))
		return
	}

	stack.push(xev.pointerToVariable())
}

func (v *Variable) pointerToVariable() *Variable {
	v.OnlyAddr = true

	typename := "*" + v.DwarfType.Common().Name
	rv := v.newVariable("", 0, &godwarf.PtrType{CommonType: godwarf.CommonType{ByteSize: int64(v.bi.Arch.PtrSize()), Name: typename}, Type: v.DwarfType}, v.mem)
	rv.Children = []Variable{*v}
	rv.loaded = true

	return rv
}

func constantUnaryOp(op token.Token, y constant.Value) (r constant.Value, err error) {
	defer func() {
		if ierr := recover(); ierr != nil {
			err = fmt.Errorf("%v", ierr)
		}
	}()
	r = constant.UnaryOp(op, y, 0)
	return
}

func constantBinaryOp(op token.Token, x, y constant.Value) (r constant.Value, err error) {
	defer func() {
		if ierr := recover(); ierr != nil {
			err = fmt.Errorf("%v", ierr)
		}
	}()
	switch op {
	case token.SHL, token.SHR:
		n, _ := constant.Uint64Val(y)
		r = constant.Shift(x, op, uint(n))
	default:
		r = constant.BinaryOp(x, op, y)
	}
	return
}

func constantCompare(op token.Token, x, y constant.Value) (r bool, err error) {
	defer func() {
		if ierr := recover(); ierr != nil {
			err = fmt.Errorf("%v", ierr)
		}
	}()
	r = constant.Compare(x, op, y)
	return
}

// Evaluates expressions: -<subexpr> and +<subexpr>
func (scope *EvalScope) evalUnary(op *evalop.Unary, stack *evalStack) {
	xv := stack.pop()

	xv.loadValue(loadSingleValue)
	if xv.Unreadable != nil {
		stack.err = xv.Unreadable
		return
	}
	if xv.FloatSpecial != 0 {
		stack.err = errOperationOnSpecialFloat
		return
	}
	if xv.Value == nil {
		stack.err = fmt.Errorf("operator %s can not be applied to %q", op.Node.Op.String(), exprToString(op.Node.X))
		return
	}
	rc, err := constantUnaryOp(op.Node.Op, xv.Value)
	if err != nil {
		stack.err = err
		return
	}
	if xv.DwarfType != nil {
		r := xv.newVariable("", 0, xv.DwarfType, scope.Mem)
		r.Value = rc
		stack.push(r)
		return
	}
	stack.push(newConstant(rc, xv.mem))
}

func negotiateType(op token.Token, xv, yv *Variable) (godwarf.Type, error) {
	if xv == nilVariable {
		return nil, negotiateTypeNil(op, yv)
	}

	if yv == nilVariable {
		return nil, negotiateTypeNil(op, xv)
	}

	if op == token.SHR || op == token.SHL {
		if xv.Value == nil || xv.Value.Kind() != constant.Int {
			return nil, fmt.Errorf("shift of type %s", xv.Kind)
		}

		switch yv.Kind {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			// ok
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if constant.Sign(yv.Value) < 0 {
				return nil, fmt.Errorf("shift count must not be negative")
			}
		default:
			return nil, fmt.Errorf("shift count type %s, must be unsigned integer", yv.Kind.String())
		}

		return xv.DwarfType, nil
	}

	if xv.DwarfType == nil && yv.DwarfType == nil {
		return nil, nil
	}

	if xv.DwarfType != nil && yv.DwarfType != nil {
		if xv.DwarfType.String() != yv.DwarfType.String() {
			return nil, fmt.Errorf("mismatched types %q and %q", xv.DwarfType.String(), yv.DwarfType.String())
		}
		return xv.DwarfType, nil
	} else if xv.DwarfType != nil && yv.DwarfType == nil {
		if err := yv.isType(xv.DwarfType, xv.Kind); err != nil {
			return nil, err
		}
		return xv.DwarfType, nil
	} else if xv.DwarfType == nil && yv.DwarfType != nil {
		if err := xv.isType(yv.DwarfType, yv.Kind); err != nil {
			return nil, err
		}
		return yv.DwarfType, nil
	}

	panic("unreachable")
}

func negotiateTypeNil(op token.Token, v *Variable) error {
	if op != token.EQL && op != token.NEQ {
		return fmt.Errorf("operator %s can not be applied to \"nil\"", op.String())
	}
	switch v.Kind {
	case reflect.Ptr, reflect.UnsafePointer, reflect.Chan, reflect.Map, reflect.Interface, reflect.Slice, reflect.Func:
		return nil
	default:
		return fmt.Errorf("can not compare %s to nil", v.Kind.String())
	}
}

func (scope *EvalScope) evalBinary(binop *evalop.Binary, stack *evalStack) {
	node := binop.Node

	yv := stack.pop()
	xv := stack.pop()

	if xv.Kind != reflect.String { // delay loading strings until we use them
		xv.loadValue(loadFullValue)
	}
	if xv.Unreadable != nil {
		stack.err = xv.Unreadable
		return
	}
	if yv.Kind != reflect.String { // delay loading strings until we use them
		yv.loadValue(loadFullValue)
	}
	if yv.Unreadable != nil {
		stack.err = yv.Unreadable
		return
	}

	if xv.FloatSpecial != 0 || yv.FloatSpecial != 0 {
		stack.err = errOperationOnSpecialFloat
		return
	}

	typ, err := negotiateType(node.Op, xv, yv)
	if err != nil {
		stack.err = err
		return
	}

	op := node.Op
	if typ != nil && (op == token.QUO) {
		_, isint := typ.(*godwarf.IntType)
		_, isuint := typ.(*godwarf.UintType)
		if isint || isuint {
			// forces integer division if the result type is integer
			op = token.QUO_ASSIGN
		}
	}

	switch op {
	case token.EQL, token.LSS, token.GTR, token.NEQ, token.LEQ, token.GEQ:
		v, err := compareOp(op, xv, yv)
		if err != nil {
			stack.err = err
			return
		}
		stack.push(newConstant(constant.MakeBool(v), xv.mem))

	default:
		if xv.Kind == reflect.String {
			xv.loadValue(loadFullValueLongerStrings)
		}
		if yv.Kind == reflect.String {
			yv.loadValue(loadFullValueLongerStrings)
		}
		if xv.Value == nil {
			stack.err = fmt.Errorf("operator %s can not be applied to %q", node.Op.String(), exprToString(node.X))
			return
		}

		if yv.Value == nil {
			stack.err = fmt.Errorf("operator %s can not be applied to %q", node.Op.String(), exprToString(node.Y))
			return
		}

		rc, err := constantBinaryOp(op, xv.Value, yv.Value)
		if err != nil {
			stack.err = err
			return
		}

		if typ == nil {
			stack.push(newConstant(rc, xv.mem))
			return
		}

		r := xv.newVariable("", 0, typ, scope.Mem)
		r.Value = rc
		switch r.Kind {
		case reflect.String:
			r.Len = xv.Len + yv.Len
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, _ := constant.Int64Val(r.Value)
			r.Value = constant.MakeInt64(int64(convertInt(uint64(n), true, typ.Size())))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, _ := constant.Uint64Val(r.Value)
			r.Value = constant.MakeUint64(convertInt(n, false, typ.Size()))
		}
		stack.push(r)
	}
}

// Compares xv to yv using operator op
// Both xv and yv must be loaded and have a compatible type (as determined by negotiateType)
func compareOp(op token.Token, xv *Variable, yv *Variable) (bool, error) {
	switch xv.Kind {
	case reflect.Bool:
		fallthrough
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fallthrough
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		fallthrough
	case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return constantCompare(op, xv.Value, yv.Value)
	case reflect.String:
		if xv.Len != yv.Len {
			switch op {
			case token.EQL:
				return false, nil
			case token.NEQ:
				return true, nil
			}
		}
		if xv.Kind == reflect.String {
			xv.loadValue(loadFullValueLongerStrings)
		}
		if yv.Kind == reflect.String {
			yv.loadValue(loadFullValueLongerStrings)
		}
		if int64(len(constant.StringVal(xv.Value))) != xv.Len || int64(len(constant.StringVal(yv.Value))) != yv.Len {
			return false, fmt.Errorf("string too long for comparison")
		}
		return constantCompare(op, xv.Value, yv.Value)
	}

	if op != token.EQL && op != token.NEQ {
		return false, fmt.Errorf("operator %s not defined on %s", op.String(), xv.Kind.String())
	}

	var eql bool
	var err error

	if xv == nilVariable {
		switch op {
		case token.EQL:
			return yv.isNil(), nil
		case token.NEQ:
			return !yv.isNil(), nil
		}
	}

	if yv == nilVariable {
		switch op {
		case token.EQL:
			return xv.isNil(), nil
		case token.NEQ:
			return !xv.isNil(), nil
		}
	}

	switch xv.Kind {
	case reflect.Ptr:
		eql = xv.Children[0].Addr == yv.Children[0].Addr
	case reflect.Array:
		if int64(len(xv.Children)) != xv.Len || int64(len(yv.Children)) != yv.Len {
			return false, fmt.Errorf("array too long for comparison")
		}
		eql, err = equalChildren(xv, yv, true)
	case reflect.Struct:
		if len(xv.Children) != len(yv.Children) {
			return false, nil
		}
		if int64(len(xv.Children)) != xv.Len || int64(len(yv.Children)) != yv.Len {
			return false, fmt.Errorf("structure too deep for comparison")
		}
		eql, err = equalChildren(xv, yv, false)
	case reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		return false, fmt.Errorf("can not compare %s variables", xv.Kind.String())
	case reflect.Interface:
		if xv.Children[0].RealType.String() != yv.Children[0].RealType.String() {
			eql = false
		} else {
			eql, err = compareOp(token.EQL, &xv.Children[0], &yv.Children[0])
		}
	default:
		return false, fmt.Errorf("unimplemented comparison of %s variables", xv.Kind.String())
	}

	if op == token.NEQ {
		return !eql, err
	}
	return eql, err
}

func (v *Variable) isNil() bool {
	switch v.Kind {
	case reflect.Ptr:
		return v.Children[0].Addr == 0
	case reflect.Interface:
		return v.Children[0].Addr == 0 && v.Children[0].Kind == reflect.Invalid
	case reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		return v.Base == 0
	}
	return false
}

func equalChildren(xv, yv *Variable, shortcircuit bool) (bool, error) {
	r := true
	for i := range xv.Children {
		eql, err := compareOp(token.EQL, &xv.Children[i], &yv.Children[i])
		if err != nil {
			return false, err
		}
		r = r && eql
		if !r && shortcircuit {
			return false, nil
		}
	}
	return r, nil
}

func (v *Variable) asInt() (int64, error) {
	if v.DwarfType == nil {
		if v.Value.Kind() != constant.Int {
			return 0, fmt.Errorf("can not convert constant %s to int", v.Value)
		}
	} else {
		v.loadValue(loadSingleValue)
		if v.Unreadable != nil {
			return 0, v.Unreadable
		}
		if _, ok := v.DwarfType.(*godwarf.IntType); !ok {
			return 0, fmt.Errorf("can not convert value of type %s to int", v.DwarfType.String())
		}
	}
	n, _ := constant.Int64Val(v.Value)
	return n, nil
}

func (v *Variable) asUint() (uint64, error) {
	if v.DwarfType == nil {
		if v.Value.Kind() != constant.Int {
			return 0, fmt.Errorf("can not convert constant %s to uint", v.Value)
		}
	} else {
		v.loadValue(loadSingleValue)
		if v.Unreadable != nil {
			return 0, v.Unreadable
		}
		if _, ok := v.DwarfType.(*godwarf.UintType); !ok {
			return 0, fmt.Errorf("can not convert value of type %s to uint", v.DwarfType.String())
		}
	}
	n, _ := constant.Uint64Val(v.Value)
	return n, nil
}

type typeConvErr struct {
	srcType, dstType godwarf.Type
}

func (err *typeConvErr) Error() string {
	return fmt.Sprintf("can not convert value of type %s to %s", err.srcType.String(), err.dstType.String())
}

func (v *Variable) isType(typ godwarf.Type, kind reflect.Kind) error {
	if v.DwarfType != nil {
		if typ == nil || !sameType(typ, v.RealType) {
			return &typeConvErr{v.DwarfType, typ}
		}
		return nil
	}

	if typ == nil {
		return nil
	}

	if v == nilVariable {
		switch kind {
		case reflect.Slice, reflect.Map, reflect.Func, reflect.Ptr, reflect.Chan, reflect.Interface:
			return nil
		default:
			return fmt.Errorf("mismatched types nil and %s", typ.String())
		}
	}

	converr := fmt.Errorf("can not convert %s constant to %s", v.Value, typ.String())

	if v.Value == nil {
		return converr
	}

	switch typ.(type) {
	case *godwarf.IntType:
		if v.Value.Kind() != constant.Int {
			return converr
		}
	case *godwarf.UintType:
		if v.Value.Kind() != constant.Int {
			return converr
		}
	case *godwarf.FloatType:
		if (v.Value.Kind() != constant.Int) && (v.Value.Kind() != constant.Float) {
			return converr
		}
	case *godwarf.BoolType:
		if v.Value.Kind() != constant.Bool {
			return converr
		}
	case *godwarf.StringType:
		if v.Value.Kind() != constant.String {
			return converr
		}
	case *godwarf.ComplexType:
		if v.Value.Kind() != constant.Complex && v.Value.Kind() != constant.Float && v.Value.Kind() != constant.Int {
			return converr
		}
	default:
		return converr
	}

	return nil
}

func sameType(t1, t2 godwarf.Type) bool {
	// Because of a bug in the go linker a type that refers to another type
	// (for example a pointer type) will usually use the typedef but rarely use
	// the non-typedef entry directly.
	// For types that we read directly from go this is fine because it's
	// consistent, however we also synthesize some types ourselves
	// (specifically pointers and slices) and we always use a reference through
	// a typedef.
	t1 = resolveTypedef(t1)
	t2 = resolveTypedef(t2)

	if tt1, isptr1 := t1.(*godwarf.PtrType); isptr1 {
		tt2, isptr2 := t2.(*godwarf.PtrType)
		if !isptr2 {
			return false
		}
		return sameType(tt1.Type, tt2.Type)
	}
	if tt1, isslice1 := t1.(*godwarf.SliceType); isslice1 {
		tt2, isslice2 := t2.(*godwarf.SliceType)
		if !isslice2 {
			return false
		}
		return sameType(tt1.ElemType, tt2.ElemType)
	}
	return t1.String() == t2.String()
}

func (v *Variable) sliceAccess(idx int) (*Variable, error) {
	wrong := false
	if v.Flags&VariableCPtr == 0 {
		wrong = idx < 0 || int64(idx) >= v.Len
	} else {
		wrong = idx < 0
	}
	if wrong {
		return nil, fmt.Errorf("index out of bounds")
	}
	if v.loaded {
		if v.Kind == reflect.String {
			s := constant.StringVal(v.Value)
			if idx >= len(s) {
				return nil, fmt.Errorf("index out of bounds")
			}
			r := v.newVariable("", v.Base+uint64(int64(idx)*v.stride), v.fieldType, v.mem)
			r.loaded = true
			r.Value = constant.MakeInt64(int64(s[idx]))
			return r, nil
		} else {
			if idx >= len(v.Children) {
				return nil, fmt.Errorf("index out of bounds")
			}
			return &v.Children[idx], nil
		}
	}
	mem := v.mem
	if v.Kind != reflect.Array {
		mem = DereferenceMemory(mem)
	}
	return v.newVariable("", v.Base+uint64(int64(idx)*v.stride), v.fieldType, mem), nil
}

func (v *Variable) mapAccess(idx *Variable) (*Variable, error) {
	it := v.mapIterator()
	if it == nil {
		return nil, fmt.Errorf("can not access unreadable map: %v", v.Unreadable)
	}

	lcfg := loadFullValue
	if idx.Kind == reflect.String && int64(len(constant.StringVal(idx.Value))) == idx.Len && idx.Len > int64(lcfg.MaxStringLen) {
		// If the index is a string load as much of the keys to at least match the length of the index.
		//TODO(aarzilli): when struct literals are implemented this needs to be
		//done recursively for literal struct fields.
		lcfg.MaxStringLen = int(idx.Len)
	}

	first := true
	for it.next() {
		key := it.key()
		key.loadValue(lcfg)
		if key.Unreadable != nil {
			return nil, fmt.Errorf("can not access unreadable map: %v", key.Unreadable)
		}
		if first {
			first = false
			if err := idx.isType(key.RealType, key.Kind); err != nil {
				return nil, err
			}
		}
		eql, err := compareOp(token.EQL, key, idx)
		if err != nil {
			return nil, err
		}
		if eql {
			return it.value(), nil
		}
	}
	if v.Unreadable != nil {
		return nil, v.Unreadable
	}
	// go would return zero for the map value type here, we do not have the ability to create zeroes
	return nil, fmt.Errorf("key not found")
}

// LoadResliced returns a new array, slice or map that starts at index start and contains
// up to cfg.MaxArrayValues children.
func (v *Variable) LoadResliced(start int, cfg LoadConfig) (newV *Variable, err error) {
	switch v.Kind {
	case reflect.Array, reflect.Slice:
		low, high := int64(start), int64(start+cfg.MaxArrayValues)
		if high > v.Len {
			high = v.Len
		}
		newV, err = v.reslice(low, high)
		if err != nil {
			return nil, err
		}
	case reflect.Map:
		newV = v.clone()
		newV.Children = nil
		newV.loaded = false
		newV.mapSkip = start
	default:
		return nil, fmt.Errorf("variable to reslice is not an array, slice, or map")
	}
	newV.loadValue(cfg)
	return newV, nil
}

func (v *Variable) reslice(low int64, high int64) (*Variable, error) {
	wrong := false
	cptrNeedsFakeSlice := false
	if v.Flags&VariableCPtr == 0 {
		wrong = low < 0 || low > v.Len || high < 0 || high > v.Len
	} else {
		wrong = low < 0 || high < 0
		if high == 0 {
			high = low
		}
		cptrNeedsFakeSlice = v.Kind != reflect.String
	}
	if wrong {
		return nil, fmt.Errorf("index out of bounds")
	}

	base := v.Base + uint64(low*v.stride)
	len := high - low

	if high-low < 0 {
		return nil, fmt.Errorf("index out of bounds")
	}

	typ := v.DwarfType
	if _, isarr := v.DwarfType.(*godwarf.ArrayType); isarr || cptrNeedsFakeSlice {
		typ = godwarf.FakeSliceType(v.fieldType)
	}

	mem := v.mem
	if v.Kind != reflect.Array {
		mem = DereferenceMemory(mem)
	}

	r := v.newVariable("", 0, typ, mem)
	r.Cap = len
	r.Len = len
	r.Base = base
	r.stride = v.stride
	r.fieldType = v.fieldType
	r.Flags = v.Flags
	r.reg = v.reg

	return r, nil
}

// findMethod finds method mname in the type of variable v
func (v *Variable) findMethod(mname string) (*Variable, error) {
	if _, isiface := v.RealType.(*godwarf.InterfaceType); isiface {
		v.loadInterface(0, false, loadFullValue)
		if v.Unreadable != nil {
			return nil, v.Unreadable
		}
		return v.Children[0].findMethod(mname)
	}

	queue := []*Variable{v}
	seen := map[string]struct{}{}

	for len(queue) > 0 {
		v := queue[0]
		queue = append(queue[:0], queue[1:]...)
		if _, isseen := seen[v.RealType.String()]; isseen {
			continue
		}
		seen[v.RealType.String()] = struct{}{}

		typ := v.DwarfType
		ptyp, isptr := typ.(*godwarf.PtrType)
		if isptr {
			typ = ptyp.Type
		}

		typePath := typ.Common().Name
		dot := strings.LastIndex(typePath, ".")
		if dot < 0 {
			// probably just a C type
			continue
		}

		pkg := typePath[:dot]
		receiver := typePath[dot+1:]

		//TODO(aarzilli): support generic functions?

		if fns := v.bi.LookupFunc()[fmt.Sprintf("%s.%s.%s", pkg, receiver, mname)]; len(fns) == 1 {
			r, err := functionToVariable(fns[0], v.bi, v.mem)
			if err != nil {
				return nil, err
			}
			if isptr {
				r.Children = append(r.Children, *(v.maybeDereference()))
			} else {
				r.Children = append(r.Children, *v)
			}
			return r, nil
		}

		if fns := v.bi.LookupFunc()[fmt.Sprintf("%s.(*%s).%s", pkg, receiver, mname)]; len(fns) == 1 {
			r, err := functionToVariable(fns[0], v.bi, v.mem)
			if err != nil {
				return nil, err
			}
			if isptr {
				r.Children = append(r.Children, *v)
			} else {
				r.Children = append(r.Children, *(v.pointerToVariable()))
			}
			return r, nil
		}

		// queue embedded fields for search
		structVar := v.maybeDereference()
		structVar.Name = v.Name
		if structVar.Unreadable != nil {
			return structVar, nil
		}
		switch t := structVar.RealType.(type) {
		case *godwarf.StructType:
			for _, field := range t.Field {
				if field.Embedded {
					embeddedVar, err := structVar.toField(field)
					if err != nil {
						return nil, err
					}
					queue = append(queue, embeddedVar)
				}
			}
		}
	}

	return nil, nil
}

func functionToVariable(fn *Function, bi *BinaryInfo, mem MemoryReadWriter) (*Variable, error) {
	typ, err := fn.fakeType(bi, true)
	if err != nil {
		return nil, err
	}
	v := newVariable(fn.Name, 0, typ, bi, mem)
	v.Value = constant.MakeString(fn.Name)
	v.loaded = true
	v.Base = fn.Entry
	return v, nil
}

func fakeArrayType(n uint64, fieldType godwarf.Type) godwarf.Type {
	stride := alignAddr(fieldType.Common().ByteSize, fieldType.Align())
	return &godwarf.ArrayType{
		CommonType: godwarf.CommonType{
			ReflectKind: reflect.Array,
			ByteSize:    int64(n) * stride,
			Name:        fmt.Sprintf("[%d]%s", n, fieldType.String())},
		Type:          fieldType,
		StrideBitSize: stride * 8,
		Count:         int64(n)}
}

var errMethodEvalUnsupported = errors.New("evaluating methods not supported on this version of Go")

func (fn *Function) fakeType(bi *BinaryInfo, removeReceiver bool) (*godwarf.FuncType, error) {
	if producer := bi.Producer(); producer == "" || !goversion.ProducerAfterOrEqual(producer, 1, 10) {
		// versions of Go prior to 1.10 do not distinguish between parameters and
		// return values, therefore we can't use a subprogram DIE to derive a
		// function type.
		return nil, errMethodEvalUnsupported
	}
	_, formalArgs, err := funcCallArgs(fn, bi, true)
	if err != nil {
		return nil, err
	}

	// Only try and remove the receiver if it is actually being passed in as a formal argument.
	// In the case of:
	//
	// func (_ X) Method() { ... }
	//
	// that would not be true, the receiver is not used and thus
	// not being passed in as a formal argument.
	//
	// TODO(derekparker) This, I think, creates a new bug where
	// if the receiver is not passed in as a formal argument but
	// there are other arguments, such as:
	//
	// func (_ X) Method(i int) { ... }
	//
	// The first argument 'i int' will be removed. We must actually detect
	// here if the receiver is being used. While this is a bug, it's not a
	// functional bug, it only affects the string representation of the fake
	// function type we create. It's not really easy to tell here if we use
	// the receiver or not. Perhaps we should not perform this manipulation at all?
	if removeReceiver && len(formalArgs) > 0 {
		formalArgs = formalArgs[1:]
	}

	args := make([]string, 0, len(formalArgs))
	rets := make([]string, 0, len(formalArgs))

	for _, formalArg := range formalArgs {
		var s string
		if strings.HasPrefix(formalArg.name, "~") {
			s = formalArg.typ.String()
		} else {
			s = fmt.Sprintf("%s %s", formalArg.name, formalArg.typ.String())
		}
		if formalArg.isret {
			rets = append(rets, s)
		} else {
			args = append(args, s)
		}
	}

	argstr := strings.Join(args, ", ")
	var retstr string
	switch len(rets) {
	case 0:
		retstr = ""
	case 1:
		retstr = " " + rets[0]
	default:
		retstr = " (" + strings.Join(rets, ", ") + ")"
	}
	return &godwarf.FuncType{
		CommonType: godwarf.CommonType{
			Name:        "func(" + argstr + ")" + retstr,
			ReflectKind: reflect.Func,
		},
		//TODO(aarzilli): at the moment we aren't using the ParamType and
		// ReturnType fields of FuncType anywhere (when this is returned to the
		// client it's first converted to a string and the function calling code
		// reads the subroutine entry because it needs to know the stack offsets).
		// If we start using them they should be filled here.
	}, nil
}

func validRegisterName(s string) string {
	for len(s) > 0 && s[0] == '_' {
		s = s[1:]
	}
	for i := range s {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'A' || s[i] > 'Z') {
			return ""
		}
	}
	return s
}
