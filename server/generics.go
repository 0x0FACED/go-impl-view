package main

import "go/types"

type observedType struct {
	named   *types.Named
	pointer *types.Pointer
	iface   *types.Interface
}

func observeType(named *types.Named) observedType {
	observation := observedType{named: named}
	observation.iface, _ = named.Underlying().(*types.Interface)
	if observation.iface == nil {
		observation.pointer = types.NewPointer(named)
	}
	return observation
}

// Keep the actual instantiated types. Implements checks named argument and
// method identities; rebuilding an instance in another test variant's context
// would erase those distinctions. Unrelated imports need not share identities.
func observedImplementation(candidates, interfaces []observedType) bool {
	for _, candidate := range candidates {
		for _, iface := range interfaces {
			if types.Implements(candidate.named, iface.iface) || types.Implements(candidate.pointer, iface.iface) {
				return true
			}
		}
	}
	return false
}

type closedTypeChecker struct {
	seen              map[types.Type]bool
	localGenericTypes map[*types.Named]bool
}

func (checker *closedTypeChecker) isClosed(typ types.Type) bool {
	// Do not cache a positive result reached through a recursive back edge: a
	// different branch of that cycle may still contain a free type parameter.
	clear(checker.seen)
	return walkTypes(typ, checker.seen, func(typ types.Type) bool {
		switch typ := typ.(type) {
		case *types.TypeParam:
			return false
		case *types.Named:
			return !checker.localGenericTypes[typ.Origin()] &&
				(typ.TypeParams().Len() == 0 || typ.TypeArgs().Len() == typ.TypeParams().Len())
		case *types.Signature:
			return typ.TypeParams().Len() == 0
		}
		return true
	})
}

// walkTypes includes instantiated underlying types and ordinary method
// signatures: an observed Outer[int] may expose Inner[int] without a separate
// source spelling. Uninstantiated generic methods are independent binders, not
// observations of their possible instantiations; actual generic method uses are
// collected from Instances and expression types. Receiver binders and constraints
// likewise do not supply additional concrete type observations.
func walkTypes(typ types.Type, seen map[types.Type]bool, visit func(types.Type) bool) bool {
	if typ == nil || seen[typ] {
		return true
	}
	seen[typ] = true
	if !visit(typ) {
		return false
	}
	walk := func(child types.Type) bool { return walkTypes(child, seen, visit) }
	switch typ := typ.(type) {
	case *types.Alias:
		return walk(types.Unalias(typ))
	case *types.Named:
		for i := range typ.TypeArgs().Len() {
			if !walk(typ.TypeArgs().At(i)) {
				return false
			}
		}
		if !walk(typ.Underlying()) {
			return false
		}
		for i := range typ.NumMethods() {
			signature := typ.Method(i).Type().(*types.Signature)
			if signature.TypeParams().Len() > 0 {
				continue
			}
			if !walk(signature) {
				return false
			}
		}
	case *types.Pointer:
		return walk(typ.Elem())
	case *types.Array:
		return walk(typ.Elem())
	case *types.Slice:
		return walk(typ.Elem())
	case *types.Map:
		return walk(typ.Key()) && walk(typ.Elem())
	case *types.Chan:
		return walk(typ.Elem())
	case *types.Struct:
		for i := range typ.NumFields() {
			if !walk(typ.Field(i).Type()) {
				return false
			}
		}
	case *types.Tuple:
		for i := range typ.Len() {
			if !walk(typ.At(i).Type()) {
				return false
			}
		}
	case *types.Signature:
		if typ.TypeParams().Len() > 0 {
			return true
		}
		return walk(typ.Params()) && walk(typ.Results())
	case *types.Interface:
		for i := range typ.NumExplicitMethods() {
			if !walk(typ.ExplicitMethod(i).Type()) {
				return false
			}
		}
		for i := range typ.NumEmbeddeds() {
			if !walk(typ.EmbeddedType(i)) {
				return false
			}
		}
	case *types.Union:
		for i := range typ.Len() {
			if !walk(typ.Term(i).Type()) {
				return false
			}
		}
	}
	return true
}
