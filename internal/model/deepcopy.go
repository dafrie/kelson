package model

import "reflect"

// Deep copies of the two spec structs, for the CRD wrappers in
// api/kelson/v1alpha1 (ADR-0027 decision 3).
//
// # Why these live here and not with the wrappers
//
// A Go method can only be declared in the package that owns the receiver type,
// so `ProjectSpec.DeepCopyInto` has exactly one possible home and it is this
// one. api/kelson/v1alpha1 embeds these structs as the `Spec` of a custom
// resource, and controller-runtime's client requires every object it stores in
// its cache to be deep-copyable — a shallow copy would let one caller's mutation
// of a component slice show up in another caller's cached object.
//
// # Why they are hand-written and reflective rather than controller-gen output
//
// api/kelson/v1alpha1's own zz_generated.deepcopy.go *is* controller-gen output
// (the one build tool ADR-0027 decision 3 adopts). controller-gen cannot be
// pointed at this package: `Component.Values` is a `map[string]any`, and
// controller-gen's deepcopy backend panics on an `interface{}` map value rather
// than declining it (sigs.k8s.io/controller-tools@v0.19.0,
// pkg/deepcopy/traverse.go genMapDeepCopy). The choices were to change the
// authoring model to suit a generator — `Values` is verbatim chart values and
// genuinely is an arbitrary document — or to write the two entry points by hand.
//
// Written by hand, they are reflective rather than field-by-field on purpose.
// The alternative is ~300 lines mirroring every struct in project.go and
// environment.go, which is a second copy of the model that nothing checks: a
// field added to `Component` without a matching line there would be silently
// dropped from every copy, which is exactly the class of bug a deep copy exists
// to prevent. The reflective walk cannot miss a field, and deepcopy_test.go
// asserts that on a fully populated spec.
//
// The fence is intact: this file imports `reflect` and nothing else, so
// internal/model still holds no Kubernetes dependency and the renderer can still
// import it (issue #20, .golangci.yml `main` rule).

// DeepCopyInto copies the receiver into out, sharing no memory with it.
func (in *ProjectSpec) DeepCopyInto(out *ProjectSpec) { deepCopyInto(in, out) }

// DeepCopy returns a deep copy of the receiver. The signature is the one
// controller-gen looks for when it decides not to recurse into a type
// (hasDeepCopyMethod): pointer receiver, no arguments, pointer result.
func (in *ProjectSpec) DeepCopy() *ProjectSpec {
	if in == nil {
		return nil
	}
	out := new(ProjectSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out, sharing no memory with it.
func (in *EnvironmentSpec) DeepCopyInto(out *EnvironmentSpec) { deepCopyInto(in, out) }

// DeepCopy returns a deep copy of the receiver.
func (in *EnvironmentSpec) DeepCopy() *EnvironmentSpec {
	if in == nil {
		return nil
	}
	out := new(EnvironmentSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out, sharing no memory with it.
func (in *GitConnectionSpec) DeepCopyInto(out *GitConnectionSpec) { deepCopyInto(in, out) }

// DeepCopy returns a deep copy of the receiver.
func (in *GitConnectionSpec) DeepCopy() *GitConnectionSpec {
	if in == nil {
		return nil
	}
	out := new(GitConnectionSpec)
	in.DeepCopyInto(out)
	return out
}

func deepCopyInto[T any](in, out *T) {
	if in == nil || out == nil {
		return
	}
	reflect.ValueOf(out).Elem().Set(deepCopyValue(reflect.ValueOf(in).Elem()))
}

// deepCopyValue returns a copy of v that shares no mutable memory with it.
//
// Nil pointers, slices and maps are returned as they are, so a copy of an unset
// optional field stays unset — "absent" and "present but empty" are different
// documents, and round-tripping one into the other through a deep copy would
// change what the spec says.
//
// Scalars (including strings, which are immutable) fall through to the default
// branch and are returned by value.
func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out.SetMapIndex(deepCopyValue(iter.Key()), deepCopyValue(iter.Value()))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := range v.NumField() {
			// Unexported fields cannot be set through reflection. The model has
			// none — every field of every spec type is part of the authored
			// document — and deepcopy_test.go fails if one ever appears, rather
			// than letting this branch drop it silently.
			if !out.Field(i).CanSet() {
				continue
			}
			out.Field(i).Set(deepCopyValue(v.Field(i)))
		}
		return out
	default:
		return v
	}
}
