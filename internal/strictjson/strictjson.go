// Package strictjson is the single strict JSON helper for every contract
// Bellona reads or writes: events, the snapshot, the Run manifest,
// configuration, typed artifacts and evaluation records (SPEC §8, R-ART-01).
//
// # Contract convention
//
// A contract is a Go struct whose exported fields all carry an explicit json
// name. A field is optional if and only if its json tag carries omitzero;
// every other field is required and must be present in the JSON object. JSON
// null is never valid anywhere: an unset or unknown optional value is an
// absent member, never a null.
//
// Decode rejects invalid syntax, trailing values, duplicate member names,
// unknown members, missing required members, null, values of the wrong type
// and values for which Validate fails. Each rejection wraps a distinct
// sentinel error and carries the RFC 6901 JSON Pointer of the offending
// location. Encode and EncodeIndent produce deterministic bytes and refuse to
// emit null. CheckContractType verifies the convention for a Go type; every
// contract type in the module is expected to pass it in a test.
//
// The package uses encoding/json/v2 and encoding/json/jsontext only.
package strictjson

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"encoding/json/jsontext"
	json "encoding/json/v2"
)

// Validator is implemented by contract types that have constraints beyond
// their shape. Decode calls Validate on the decoded top-level value only: the
// Validate method of an outer type is responsible for validating the nested
// values that need it.
type Validator interface {
	Validate() error
}

// Sentinel errors. Every error returned by Decode, Encode and EncodeIndent is
// an *Error whose Unwrap result matches exactly one of them under errors.Is.
var (
	// ErrInvalidJSON reports empty input or a syntax error, including
	// invalid UTF-8 in a string.
	ErrInvalidJSON = errors.New("invalid JSON")
	// ErrTrailingData reports any data after the first top-level value
	// other than whitespace.
	ErrTrailingData = errors.New("trailing data after the top-level value")
	// ErrDuplicateMember reports an object with two members of the same name.
	ErrDuplicateMember = errors.New("duplicate object member")
	// ErrUnknownMember reports an object member that the Go type does not
	// declare. Member names are matched case-sensitively.
	ErrUnknownMember = errors.New("unknown object member")
	// ErrMissingMember reports a required member that is absent.
	ErrMissingMember = errors.New("missing required object member")
	// ErrNull reports a JSON null.
	ErrNull = errors.New("null is not allowed")
	// ErrInvalidValue reports a value of the wrong type or kind, a value
	// that its own UnmarshalText or UnmarshalJSON rejected, or a failed
	// Validate.
	ErrInvalidValue = errors.New("invalid value")
)

// Error is the error type of this package.
type Error struct {
	// Pointer is the RFC 6901 JSON Pointer of the offending location. The
	// empty string is the document root.
	Pointer string
	// Err is the sentinel that classifies the error. When the error has an
	// underlying cause, errors.Is also matches the cause.
	Err error
	// Detail is a human-readable explanation. It may be empty.
	Detail string
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("strictjson: ")
	if e.Err != nil {
		b.WriteString(e.Err.Error())
	} else {
		b.WriteString("error")
	}
	if e.Pointer != "" {
		b.WriteString(" at ")
		b.WriteString(strconv.Quote(e.Pointer))
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Unwrap returns Err so that errors.Is matches the sentinels.
func (e *Error) Unwrap() error { return e.Err }

// withCause is a sentinel that also matches the error that caused it, while
// its text stays the text of the sentinel.
type withCause struct{ sentinel, cause error }

func (w withCause) Error() string   { return w.sentinel.Error() }
func (w withCause) Unwrap() []error { return []error{w.sentinel, w.cause} }

// Decode decodes data into a new value of type T under the strict rules of
// the package documentation. On every error it returns the zero value of T,
// never a partially filled one, and an *Error.
func Decode[T any](data []byte) (T, error) {
	var zero T
	if err := scan(data); err != nil {
		return zero, err
	}
	var v T
	if err := json.Unmarshal(data, &v, json.RejectUnknownMembers(true)); err != nil {
		return zero, unmarshalError(err)
	}
	if err := checkRequired(data, reflect.TypeFor[T]()); err != nil {
		return zero, err
	}
	if val, ok := any(&v).(Validator); ok {
		if err := val.Validate(); err != nil {
			return zero, validationError(err)
		}
	}
	return v, nil
}

// Encode returns the compact, deterministic JSON encoding of v. Map members
// are sorted by name, so the same value always yields the same bytes. It
// returns an error wrapping ErrNull instead of emitting a null, which happens
// for a nil pointer or a zero jsontext.Value that lacks omitzero. A nil slice
// or map is encoded as [] or {}.
func Encode(v any) ([]byte, error) {
	return encode(v)
}

// EncodeIndent is like Encode with two-space indentation and a single
// trailing newline. A jsontext.Value member is indented with the rest.
func EncodeIndent(v any) ([]byte, error) {
	out, err := encode(v, jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func encode(v any, opts ...json.Options) ([]byte, error) {
	opts = append([]json.Options{json.Deterministic(true)}, opts...)
	out, err := json.Marshal(v, opts...)
	if err != nil {
		e := &Error{Err: withCause{ErrInvalidValue, err}, Detail: err.Error()}
		var se *json.SemanticError
		if errors.As(err, &se) {
			e.Pointer = string(se.JSONPointer)
		}
		return nil, e
	}
	if err := scan(out); err != nil {
		return nil, err
	}
	return out, nil
}

// scan reads every token of data and returns an *Error for the first
// violation of the syntactic rules: empty or invalid input, a duplicate
// member name, a null, or data after the first top-level value.
func scan(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	depth := 0
	done := false
	var end int64 // byte offset at which the first top-level value ends
	for {
		tok, err := dec.ReadToken()
		if done {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return &Error{
				Err:    ErrTrailingData,
				Detail: fmt.Sprintf("the first top-level value ends at byte %d", end),
			}
		}
		if err != nil {
			return syntaxError(dec, err)
		}
		switch tok.Kind() {
		case 'n':
			return &Error{
				Pointer: string(dec.StackPointer()),
				Err:     ErrNull,
				Detail:  "an unset optional value must be an absent member",
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		if depth == 0 {
			done = true
			end = dec.InputOffset()
		}
	}
}

func syntaxError(dec *jsontext.Decoder, err error) *Error {
	if errors.Is(err, io.EOF) {
		return &Error{Err: ErrInvalidJSON, Detail: "empty input"}
	}
	e := &Error{Err: ErrInvalidJSON, Pointer: string(dec.StackPointer()), Detail: err.Error()}
	if errors.Is(err, jsontext.ErrDuplicateName) {
		e.Err = ErrDuplicateMember
	}
	var se *jsontext.SyntacticError
	if errors.As(err, &se) && se.JSONPointer != "" {
		e.Pointer = string(se.JSONPointer)
	}
	return e
}

func unmarshalError(err error) *Error {
	e := &Error{Err: withCause{ErrInvalidValue, err}, Detail: err.Error()}
	var se *json.SemanticError
	if errors.As(err, &se) {
		e.Pointer = string(se.JSONPointer)
	}
	var syn *jsontext.SyntacticError
	switch {
	case errors.Is(err, json.ErrUnknownName):
		e.Err = ErrUnknownMember
	case errors.As(err, &syn):
		e.Err = ErrInvalidJSON
		e.Pointer = string(syn.JSONPointer)
	}
	return e
}

func validationError(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return &Error{Err: withCause{ErrInvalidValue, err}, Detail: err.Error()}
}

// node is the shape of a JSON document: objects with their members and arrays
// with their elements. Scalars are not kept, so that no number is ever parsed
// (a raw jsontext.Value may hold a number that no Go type can represent).
type node struct {
	kind    byte // '{' for an object, '[' for an array, 0 for a scalar
	members map[string]*node
	elems   []*node
}

// readNode reads the next value of dec into a node. The document has already
// passed scan, so a failure here is not expected.
func readNode(dec *jsontext.Decoder) (*node, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case '{':
		n := &node{kind: '{', members: make(map[string]*node)}
		for dec.PeekKind() != '}' {
			nameTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			name := nameTok.String() // read before the next call voids the token
			child, err := readNode(dec)
			if err != nil {
				return nil, err
			}
			n.members[name] = child
		}
		_, err := dec.ReadToken() // the closing brace
		return n, err
	case '[':
		n := &node{kind: '['}
		for dec.PeekKind() != ']' {
			child, err := readNode(dec)
			if err != nil {
				return nil, err
			}
			n.elems = append(n.elems, child)
		}
		_, err := dec.ReadToken() // the closing bracket
		return n, err
	}
	return &node{}, nil
}

// checkRequired verifies that every required member of t is present in data.
func checkRequired(data []byte, t reflect.Type) error {
	doc, err := readNode(jsontext.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return &Error{Err: withCause{ErrInvalidJSON, err}, Detail: err.Error()}
	}
	return walkRequired(t, doc, "")
}

// walkRequired walks the Go type t alongside the document node n. A mismatch
// between the two has already been reported by json.Unmarshal, so a value of
// an unexpected shape ends the walk without an error.
func walkRequired(t reflect.Type, n *node, ptr jsontext.Pointer) error {
	if isLeaf(t) {
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return walkRequired(t.Elem(), n, ptr)
	case reflect.Struct:
		if n.kind != '{' {
			return nil
		}
		for _, f := range structFields(t) {
			at := ptr.AppendToken(f.name)
			member, present := n.members[f.name]
			if !present {
				if f.optional {
					continue
				}
				return &Error{
					Pointer: string(at),
					Err:     ErrMissingMember,
					Detail:  fmt.Sprintf("member %q of %s is required", f.name, t),
				}
			}
			if err := walkRequired(f.typ, member, at); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if n.kind != '[' {
			return nil
		}
		for i, elem := range n.elems {
			if err := walkRequired(t.Elem(), elem, ptr.AppendToken(strconv.Itoa(i))); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.kind != '{' {
			return nil
		}
		for _, name := range slices.Sorted(maps.Keys(n.members)) {
			if err := walkRequired(t.Elem(), n.members[name], ptr.AppendToken(name)); err != nil {
				return err
			}
		}
	}
	return nil
}

type fieldInfo struct {
	name     string
	typ      reflect.Type
	optional bool
}

// structFields lists the members that encoding/json/v2 maps to the fields of
// the struct type t, flattening embedded structs without an explicit name.
func structFields(t reflect.Type) []fieldInfo {
	var out []fieldInfo
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !isLeaf(ft) {
				out = append(out, structFields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, fieldInfo{
			name:     name,
			typ:      f.Type,
			optional: slices.Contains(strings.Split(opts, ","), "omitzero"),
		})
	}
	return out
}

var (
	jsonUnmarshalerType     = reflect.TypeFor[json.Unmarshaler]()
	jsonUnmarshalerFromType = reflect.TypeFor[json.UnmarshalerFrom]()
	textUnmarshalerType     = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// isLeaf reports whether t decodes itself: time.Time, jsontext.Value and every
// type whose pointer implements json.Unmarshaler, json.UnmarshalerFrom or
// encoding.TextUnmarshaler. The members of a leaf are not part of the
// contract that this package checks. A type that only implements Validator is
// not a leaf.
func isLeaf(t reflect.Type) bool {
	for _, c := range [...]reflect.Type{t, reflect.PointerTo(t)} {
		if c.Implements(jsonUnmarshalerType) ||
			c.Implements(jsonUnmarshalerFromType) ||
			c.Implements(textUnmarshalerType) {
			return true
		}
	}
	return false
}

// CheckContractType verifies that t, and every struct type reachable from it
// through pointers, slices, arrays and map values, follows the contract
// convention of the package documentation. Leaf types (see Decode) are not
// descended into. It returns nil, or an error that joins every violation:
//
//   - an exported field without an explicit, non-empty json name, or a field
//     that is hidden with json:"-";
//   - an embedded field;
//   - an unexported field;
//   - the omitempty option;
//   - a pointer, map or interface field without omitzero, because its zero
//     value would be encoded as null or would hide an absent member;
//   - a map whose key type is not a string.
func CheckContractType(t reflect.Type) error {
	c := &contractChecker{seen: make(map[reflect.Type]bool)}
	c.walk(t)
	return errors.Join(c.violations...)
}

type contractChecker struct {
	seen       map[reflect.Type]bool
	violations []error
}

func (c *contractChecker) violate(where, format string, args ...any) {
	c.violations = append(c.violations, fmt.Errorf("%s: %s", where, fmt.Sprintf(format, args...)))
}

func (c *contractChecker) walk(t reflect.Type) {
	if isLeaf(t) {
		return
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		c.walk(t.Elem())
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			c.violate(t.String(), "map key type %s is not a string", t.Key())
		}
		c.walk(t.Elem())
	case reflect.Struct:
		if c.seen[t] {
			return
		}
		c.seen[t] = true
		for i := range t.NumField() {
			c.field(t, t.Field(i))
		}
	}
}

func (c *contractChecker) field(owner reflect.Type, f reflect.StructField) {
	where := owner.String() + "." + f.Name
	switch {
	case f.Anonymous:
		c.violate(where, "embedded fields are not allowed in a contract type")
		return
	case !f.IsExported():
		c.violate(where, "unexported fields are not allowed in a contract type")
		return
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		c.violate(where, `fields hidden with json:"-" are not allowed in a contract type`)
		return
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		c.violate(where, "exported field has no explicit json name")
	}
	options := strings.Split(opts, ",")
	if slices.Contains(options, "omitempty") {
		c.violate(where, "the omitempty option is not allowed; use omitzero for optional fields")
	}
	var kind string
	switch f.Type.Kind() {
	case reflect.Pointer:
		kind = "pointer"
	case reflect.Map:
		kind = "map"
	case reflect.Interface:
		kind = "interface"
	}
	if kind != "" && !slices.Contains(options, "omitzero") {
		c.violate(where, "a %s field needs omitzero", kind)
	}
	c.walk(f.Type)
}
