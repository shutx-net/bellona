package strictjson

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"encoding/json/jsontext"
)

// leafKind is a TextUnmarshaler leaf that accepts "a" and "b".
type leafKind string

func (k leafKind) MarshalText() ([]byte, error) {
	if k != "a" && k != "b" {
		return nil, errors.New("leafKind must be a or b")
	}
	return []byte(k), nil
}

func (k *leafKind) UnmarshalText(b []byte) error {
	if s := string(b); s != "a" && s != "b" {
		return errors.New("leafKind must be a or b")
	}
	*k = leafKind(b)
	return nil
}

type child struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ref is an optional value struct; the zero value is absent.
type ref struct {
	Path string `json:"path"`
}

func (r ref) IsZero() bool { return r.Path == "" }

type document struct {
	Name   string           `json:"name"`
	Child  child            `json:"child"`
	Items  []child          `json:"items"`
	ByName map[string]child `json:"byName,omitzero"`
	Opt    *child           `json:"opt,omitzero"`
	Ref    ref              `json:"ref,omitzero"`
	Kind   leafKind         `json:"kind"`
	Raw    jsontext.Value   `json:"raw,omitzero"`
	Tags   []string         `json:"tags"`
	At     time.Time        `json:"at,omitzero"`
}

const (
	mName   = `"name":"n"`
	mChild  = `"child":{"name":"c","count":1}`
	mItems  = `"items":[{"name":"i","count":2}]`
	mKind   = `"kind":"a"`
	mTags   = `"tags":["x"]`
	mByName = `"byName":{"k":{"name":"v","count":3}}`
)

func obj(members ...string) string { return "{" + strings.Join(members, ",") + "}" }

func validDoc() string { return obj(mName, mChild, mItems, mKind, mTags) }

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    error
		pointer string
	}{
		{"empty input", "", ErrInvalidJSON, ""},
		{"whitespace only", " \n\t", ErrInvalidJSON, ""},
		{"invalid syntax", `{"name":`, ErrInvalidJSON, "/name"},
		{"not json", `not json`, ErrInvalidJSON, ""},
		{"invalid utf-8", "{\"name\":\"\xff\"}", ErrInvalidJSON, "/name"},
		{"trailing object", validDoc() + `{}`, ErrTrailingData, ""},
		{"trailing object after space", validDoc() + " {}", ErrTrailingData, ""},
		{"trailing garbage", validDoc() + ` x`, ErrTrailingData, ""},
		{"trailing comma", validDoc() + `,`, ErrTrailingData, ""},
		{"duplicate top level", obj(mName, mName, mChild, mItems, mKind, mTags), ErrDuplicateMember, "/name"},
		{"duplicate nested", obj(mName, `"child":{"name":"c","name":"d","count":1}`, mItems, mKind, mTags), ErrDuplicateMember, "/child/name"},
		{"duplicate in map value", obj(mName, mChild, mItems, mKind, mTags, `"byName":{"k":{"name":"v","count":3,"count":4}}`), ErrDuplicateMember, "/byName/k/count"},
		{"duplicate with escape", obj(mName, `"name":"m"`, mChild, mItems, mKind, mTags), ErrDuplicateMember, "/name"},
		{"unknown top level", obj(mName, mChild, mItems, mKind, mTags, `"extra":1`), ErrUnknownMember, "/extra"},
		{"unknown in nested struct", obj(mName, `"child":{"name":"c","count":1,"x":1}`, mItems, mKind, mTags), ErrUnknownMember, "/child/x"},
		{"unknown in slice element", obj(mName, mChild, `"items":[{"name":"i","count":2,"x":1}]`, mKind, mTags), ErrUnknownMember, "/items/0/x"},
		{"unknown in map value", obj(mName, mChild, mItems, mKind, mTags, `"byName":{"k":{"name":"v","count":3,"x":1}}`), ErrUnknownMember, "/byName/k/x"},
		{"unknown in optional pointer", obj(mName, mChild, mItems, mKind, mTags, `"opt":{"name":"o","count":1,"x":1}`), ErrUnknownMember, "/opt/x"},
		{"case mismatch is unknown", obj(`"Name":"n"`, mChild, mItems, mKind, mTags), ErrUnknownMember, "/Name"},
		{"missing top level", obj(mChild, mItems, mKind, mTags), ErrMissingMember, "/name"},
		{"missing required slice", obj(mName, mChild, mItems, mKind), ErrMissingMember, "/tags"},
		{"missing in nested struct", obj(mName, `"child":{"name":"c"}`, mItems, mKind, mTags), ErrMissingMember, "/child/count"},
		{"missing in slice element", obj(mName, mChild, `"items":[{"name":"i","count":2},{"name":"j"}]`, mKind, mTags), ErrMissingMember, "/items/1/count"},
		{"missing in map value", obj(mName, mChild, mItems, mKind, mTags, `"byName":{"k":{"name":"v"}}`), ErrMissingMember, "/byName/k/count"},
		{"missing in optional pointer", obj(mName, mChild, mItems, mKind, mTags, `"opt":{"name":"o"}`), ErrMissingMember, "/opt/count"},
		{"missing in optional value struct", obj(mName, mChild, mItems, mKind, mTags, `"ref":{}`), ErrMissingMember, "/ref/path"},
		{"empty object", `{}`, ErrMissingMember, "/name"},
		{"null document", `null`, ErrNull, ""},
		{"null required field", obj(`"name":null`, mChild, mItems, mKind, mTags), ErrNull, "/name"},
		{"null required struct", obj(mName, `"child":null`, mItems, mKind, mTags), ErrNull, "/child"},
		{"null optional pointer", obj(mName, mChild, mItems, mKind, mTags, `"opt":null`), ErrNull, "/opt"},
		{"null slice", obj(mName, mChild, mItems, mKind, `"tags":null`), ErrNull, "/tags"},
		{"null slice element", obj(mName, mChild, mItems, mKind, `"tags":["x",null]`), ErrNull, "/tags/1"},
		{"null map value", obj(mName, mChild, mItems, mKind, mTags, `"byName":{"k":null}`), ErrNull, "/byName/k"},
		{"null inside raw value", obj(mName, mChild, mItems, mKind, mTags, `"raw":{"a":[1,null]}`), ErrNull, "/raw/a/1"},
		{"wrong type string", obj(`"name":1`, mChild, mItems, mKind, mTags), ErrInvalidValue, "/name"},
		{"wrong type number", obj(mName, `"child":{"name":"c","count":"1"}`, mItems, mKind, mTags), ErrInvalidValue, "/child/count"},
		{"wrong type fraction", obj(mName, `"child":{"name":"c","count":1.5}`, mItems, mKind, mTags), ErrInvalidValue, "/child/count"},
		{"wrong type slice", obj(mName, mChild, `"items":{}`, mKind, mTags), ErrInvalidValue, "/items"},
		{"wrong type struct", obj(mName, `"child":[]`, mItems, mKind, mTags), ErrInvalidValue, "/child"},
		{"wrong type document", `[]`, ErrInvalidValue, ""},
		{"leaf rejects value", obj(mName, mChild, mItems, `"kind":"zzz"`, mTags), ErrInvalidValue, "/kind"},
		{"leaf rejects empty value", obj(mName, mChild, mItems, `"kind":""`, mTags), ErrInvalidValue, "/kind"},
		{"time rejects value", obj(mName, mChild, mItems, mKind, mTags, `"at":"yesterday"`), ErrInvalidValue, "/at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode[document]([]byte(tt.input))
			if !errors.Is(err, tt.want) {
				t.Fatalf("Decode(%q) error = %v, want %v", tt.input, err, tt.want)
			}
			for _, other := range []error{ErrInvalidJSON, ErrTrailingData, ErrDuplicateMember, ErrUnknownMember, ErrMissingMember, ErrNull, ErrInvalidValue} {
				if other != tt.want && errors.Is(err, other) {
					t.Errorf("Decode(%q) error %v also matches %v", tt.input, err, other)
				}
			}
			var se *Error
			if !errors.As(err, &se) {
				t.Fatalf("error %T is not an *Error", err)
			}
			if se.Pointer != tt.pointer {
				t.Errorf("Pointer = %q, want %q (%v)", se.Pointer, tt.pointer, err)
			}
			if !reflect.DeepEqual(got, document{}) {
				t.Errorf("Decode returned a non-zero value on error: %+v", got)
			}
		})
	}
}

func TestDecodeAccepts(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, d document)
	}{
		{"required members only", validDoc(), func(t *testing.T, d document) {
			if d.Name != "n" || d.Child.Count != 1 || len(d.Items) != 1 || d.Kind != "a" || len(d.Tags) != 1 {
				t.Errorf("unexpected value %+v", d)
			}
			if d.ByName != nil || d.Opt != nil || !d.Ref.IsZero() || d.Raw != nil {
				t.Errorf("optional members should be unset: %+v", d)
			}
		}},
		{"member order does not matter", obj(mTags, mKind, mItems, mChild, mName), nil},
		{"trailing whitespace", validDoc() + " \r\n\t ", nil},
		{"leading whitespace", " \n" + validDoc(), nil},
		{"empty arrays", obj(mName, mChild, `"items":[]`, mKind, `"tags":[]`), func(t *testing.T, d document) {
			if d.Items == nil || d.Tags == nil || len(d.Items) != 0 {
				t.Errorf("empty arrays should decode to non-nil empty slices: %+v", d)
			}
		}},
		{"optional members present", obj(mName, mChild, mItems, mKind, mTags, mByName,
			`"opt":{"name":"o","count":4}`, `"ref":{"path":"p"}`, `"raw":{"free":[1,"two",true]}`, `"at":"2026-01-02T03:04:05Z"`),
			func(t *testing.T, d document) {
				if d.ByName["k"].Count != 3 || d.Opt == nil || d.Opt.Name != "o" || d.Ref.Path != "p" {
					t.Errorf("unexpected optional members %+v", d)
				}
				if string(d.Raw) != `{"free":[1,"two",true]}` {
					t.Errorf("Raw = %s", d.Raw)
				}
				if !d.At.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
					t.Errorf("At = %v", d.At)
				}
			}},
		{"numbers that no Go type can hold stay in a raw value", obj(mName, mChild, mItems, mKind, mTags, `"raw":{"huge":1e999,"big":123456789012345678901234567890}`), func(t *testing.T, d document) {
			if string(d.Raw) != `{"huge":1e999,"big":123456789012345678901234567890}` {
				t.Errorf("Raw = %s", d.Raw)
			}
		}},
		{"empty optional map", obj(mName, mChild, mItems, mKind, mTags, `"byName":{}`), nil},
		{"escaped name", obj(`"name":"n"`, mChild, mItems, mKind, mTags), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Decode[document]([]byte(tt.input))
			if err != nil {
				t.Fatalf("Decode(%q) error = %v", tt.input, err)
			}
			if tt.check != nil {
				tt.check(t, d)
			}
		})
	}
}

// Every proper prefix of an object is invalid JSON, and no input may panic.
func TestDecodeEveryPrefix(t *testing.T) {
	full := obj(mName, mChild, mItems, mKind, mTags, mByName, `"opt":{"name":"o","count":4}`, `"raw":{"a":[1,"\u00e4"]}`)
	if _, err := Decode[document]([]byte(full)); err != nil {
		t.Fatalf("the full document must decode: %v", err)
	}
	for i := range len(full) {
		got, err := Decode[document]([]byte(full[:i]))
		if !errors.Is(err, ErrInvalidJSON) {
			t.Fatalf("Decode(%q) error = %v, want ErrInvalidJSON", full[:i], err)
		}
		if !reflect.DeepEqual(got, document{}) {
			t.Fatalf("Decode(%q) returned a non-zero value", full[:i])
		}
	}
}

func TestDecodeIntoSliceAndMapRoots(t *testing.T) {
	got, err := Decode[[]child]([]byte(`[{"name":"a","count":1}]`))
	if err != nil || len(got) != 1 {
		t.Fatalf("Decode slice = %v, %v", got, err)
	}
	_, err = Decode[[]child]([]byte(`[{"name":"a"}]`))
	var se *Error
	if !errors.Is(err, ErrMissingMember) || !errors.As(err, &se) || se.Pointer != "/0/count" {
		t.Fatalf("Decode slice error = %v", err)
	}
	m, err := Decode[map[string]child]([]byte(`{"x":{"name":"a","count":1}}`))
	if err != nil || m["x"].Name != "a" {
		t.Fatalf("Decode map = %v, %v", m, err)
	}
	_, err = Decode[map[string]child]([]byte(`{"x/y":{"name":"a"}}`))
	if !errors.Is(err, ErrMissingMember) || !errors.As(err, &se) || se.Pointer != "/x~1y/count" {
		t.Fatalf("Decode map error = %v (pointer must escape '/')", err)
	}
}

// checkedValue has a value-receiver Validate; checkedPointer a
// pointer-receiver one; pointedValue returns a pointed *Error.
type checkedValue struct {
	N int `json:"n"`
}

var errNegative = errors.New("n must not be negative")

func (c checkedValue) Validate() error {
	if c.N < 0 {
		return errNegative
	}
	return nil
}

type checkedPointer struct {
	N int `json:"n"`
}

func (c *checkedPointer) Validate() error {
	if c.N < 0 {
		return errNegative
	}
	return nil
}

type pointedValue struct {
	N int `json:"n"`
}

func (p pointedValue) Validate() error {
	return &Error{Pointer: "/n", Err: ErrInvalidValue, Detail: "n is wrong"}
}

func TestDecodeValidate(t *testing.T) {
	if v, err := Decode[checkedValue]([]byte(`{"n":3}`)); err != nil || v.N != 3 {
		t.Fatalf("valid value: %+v, %v", v, err)
	}
	if v, err := Decode[checkedPointer]([]byte(`{"n":3}`)); err != nil || v.N != 3 {
		t.Fatalf("valid value: %+v, %v", v, err)
	}

	v, err := Decode[checkedValue]([]byte(`{"n":-1}`))
	if !errors.Is(err, ErrInvalidValue) || !errors.Is(err, errNegative) {
		t.Fatalf("error = %v, want ErrInvalidValue wrapping the validation error", err)
	}
	if v != (checkedValue{}) {
		t.Errorf("value on error = %+v, want zero", v)
	}
	p, err := Decode[checkedPointer]([]byte(`{"n":-1}`))
	if !errors.Is(err, ErrInvalidValue) || !errors.Is(err, errNegative) || p != (checkedPointer{}) {
		t.Fatalf("pointer receiver: %+v, %v", p, err)
	}

	// A strictjson error from Validate is returned as it is, pointer included.
	q, err := Decode[pointedValue]([]byte(`{"n":1}`))
	var se *Error
	if !errors.As(err, &se) || se.Pointer != "/n" || !errors.Is(err, ErrInvalidValue) || q != (pointedValue{}) {
		t.Fatalf("pointed error = %v (%+v)", err, q)
	}

	// Structural errors are reported before Validate runs.
	if _, err := Decode[checkedValue]([]byte(`{}`)); !errors.Is(err, ErrMissingMember) {
		t.Fatalf("error = %v, want ErrMissingMember", err)
	}
}

func TestErrorText(t *testing.T) {
	_, err := Decode[document]([]byte(obj(mName, mChild, `"items":[{"name":"i"}]`, mKind, mTags)))
	want := `strictjson: missing required object member at "/items/0/count"`
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error text = %v, want prefix %q", err, want)
	}
	if got := (&Error{Err: ErrNull}).Error(); got != "strictjson: null is not allowed" {
		t.Errorf("root error text = %q", got)
	}
}

type encodeSample struct {
	N    int               `json:"n"`
	List []string          `json:"list"`
	M    map[string]string `json:"m"`
}

func TestEncode(t *testing.T) {
	m := make(map[string]string)
	for _, k := range []string{"q", "b", "z", "a", "m", "c", "x", "k", "d", "y", "e", "l"} {
		m[k] = k
	}
	v := encodeSample{N: 1, List: []string{"x"}, M: m}
	first, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := Encode(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("Encode is not deterministic:\n%s\n%s", first, again)
		}
	}
	const want = `{"n":1,"list":["x"],"m":{"a":"a","b":"b","c":"c","d":"d","e":"e","k":"k","l":"l","m":"m","q":"q","x":"x","y":"y","z":"z"}}`
	if string(first) != want {
		t.Fatalf("Encode = %s\nwant     %s", first, want)
	}
}

func TestEncodeNilSliceAndMap(t *testing.T) {
	got, err := Encode(encodeSample{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":0,"list":[],"m":{}}` {
		t.Fatalf("Encode = %s", got)
	}
}

func TestEncodeIndent(t *testing.T) {
	got, err := EncodeIndent(encodeSample{N: 1, List: []string{"x", "y"}, M: map[string]string{"b": "2", "a": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = "{\n  \"n\": 1,\n  \"list\": [\n    \"x\",\n    \"y\"\n  ],\n  \"m\": {\n    \"a\": \"1\",\n    \"b\": \"2\"\n  }\n}\n"
	if string(got) != want {
		t.Fatalf("EncodeIndent = %q\nwant         %q", got, want)
	}
	if strings.HasSuffix(string(got), "\n\n") {
		t.Error("EncodeIndent must end with a single newline")
	}
	again, _ := EncodeIndent(encodeSample{N: 1, List: []string{"x", "y"}, M: map[string]string{"b": "2", "a": "1"}})
	if string(again) != string(got) {
		t.Error("EncodeIndent is not deterministic")
	}
}

type nullablePointer struct {
	P *child `json:"p"`
}

type nullableRaw struct {
	Raw jsontext.Value `json:"raw"`
}

type invalidKindHolder struct {
	Kind leafKind `json:"kind"`
}

func TestEncodeRefusesNull(t *testing.T) {
	tests := []struct {
		name    string
		v       any
		want    error
		pointer string
	}{
		{"nil pointer without omitzero", nullablePointer{}, ErrNull, "/p"},
		{"zero jsontext.Value without omitzero", nullableRaw{}, ErrNull, "/raw"},
		{"nil value", nil, ErrNull, ""},
		{"nil pointer to struct", (*nullablePointer)(nil), ErrNull, ""},
		{"value rejected by MarshalText", invalidKindHolder{Kind: "zzz"}, ErrInvalidValue, "/kind"},
	}
	for _, tt := range tests {
		for _, enc := range []struct {
			name string
			fn   func(any) ([]byte, error)
		}{{"Encode", Encode}, {"EncodeIndent", EncodeIndent}} {
			t.Run(tt.name+"/"+enc.name, func(t *testing.T) {
				out, err := enc.fn(tt.v)
				if !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want %v", err, tt.want)
				}
				var se *Error
				if !errors.As(err, &se) || se.Pointer != tt.pointer {
					t.Errorf("error %v: pointer want %q", err, tt.pointer)
				}
				if out != nil {
					t.Errorf("output on error = %q, want nil", out)
				}
			})
		}
	}
}

func TestRoundTripRaw(t *testing.T) {
	v := document{
		Name: "raw", Kind: "a", Items: []child{}, Tags: []string{},
		Raw: jsontext.Value(`{"free":[1,"two",true]}`),
	}
	data, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode[document](data)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	// EncodeIndent indents a raw value as well, so only its content survives.
	data, err = EncodeIndent(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err = Decode[document](data)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Raw.Compact(); err != nil || string(got.Raw) != string(v.Raw) {
		t.Fatalf("raw value after indent = %s, %v", got.Raw, err)
	}
}

func TestRoundTrip(t *testing.T) {
	values := []document{
		{
			Name:  "n",
			Child: child{Name: "c", Count: 1},
			Items: []child{{Name: "i", Count: 2}},
			Kind:  "b",
			Tags:  []string{},
		},
		{
			Name:   "full",
			Child:  child{Name: "c", Count: -7},
			Items:  []child{},
			ByName: map[string]child{"k": {Name: "v", Count: 3}, "a/b": {Name: "w", Count: 4}},
			Opt:    &child{Name: "o", Count: 5},
			Ref:    ref{Path: "p"},
			Kind:   "a",
			Tags:   []string{"x", "y"},
			At:     time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
		},
	}
	for _, v := range values {
		for _, enc := range []struct {
			name string
			fn   func(any) ([]byte, error)
		}{{"Encode", Encode}, {"EncodeIndent", EncodeIndent}} {
			t.Run(v.Name+"/"+enc.name, func(t *testing.T) {
				data, err := enc.fn(v)
				if err != nil {
					t.Fatal(err)
				}
				got, err := Decode[document](data)
				if err != nil {
					t.Fatalf("Decode(%s) error = %v", data, err)
				}
				if !reflect.DeepEqual(got, v) {
					t.Fatalf("round trip changed the value:\n got  %+v\n want %+v", got, v)
				}
				again, err := enc.fn(got)
				if err != nil || string(again) != string(data) {
					t.Fatalf("re-encoding differs: %v\n%s\n%s", err, data, again)
				}
			})
		}
	}
}

// Contract types that violate exactly one rule each.
type (
	violNoName    struct{ A string }
	violEmptyName struct {
		A string `json:",omitzero"`
	}
	violEmbedded   struct{ child }
	violUnexported struct{ a string }
	violOmitEmpty  struct {
		A string `json:"a,omitempty"`
	}
	violPointer struct {
		P *child `json:"p"`
	}
	violMap struct {
		M map[string]string `json:"m"`
	}
	violInterface struct {
		I any `json:"i"`
	}
	violMapKey struct {
		M map[int]string `json:"m,omitzero"`
	}
	violHidden struct {
		A string `json:"-"`
	}
	violNested struct {
		Items []violPointer `json:"items"`
	}
	violMapValue struct {
		M map[string]violNoName `json:"m"`
	}
)

func (violUnexported) unused() string { return "" }

type recursive struct {
	Name string      `json:"name"`
	Next *recursive  `json:"next,omitzero"`
	Kids []recursive `json:"kids"`
}

func TestCheckContractTypeAccepts(t *testing.T) {
	for _, v := range []any{
		document{}, child{}, ref{}, checkedValue{}, checkedPointer{},
		&child{}, []child{}, map[string]child{}, recursive{}, time.Time{}, jsontext.Value(nil), leafKind(""),
	} {
		if err := CheckContractType(reflect.TypeOf(v)); err != nil {
			t.Errorf("CheckContractType(%T) = %v, want nil", v, err)
		}
	}
}

func TestCheckContractTypeRejects(t *testing.T) {
	tests := []struct {
		v    any
		want string
	}{
		{violNoName{}, "no explicit json name"},
		{violEmptyName{}, "no explicit json name"},
		{violEmbedded{}, "embedded"},
		{violUnexported{}, "unexported"},
		{violOmitEmpty{}, "omitempty"},
		{violPointer{}, "pointer field needs omitzero"},
		{violMap{}, "map field needs omitzero"},
		{violInterface{}, "interface field needs omitzero"},
		{violMapKey{}, "not a string"},
		{violHidden{}, `json:"-"`},
		{violNested{}, "pointer field needs omitzero"},
		{violMapValue{}, "no explicit json name"},
		{[]violPointer{}, "pointer field needs omitzero"},
		{map[int]child{}, "not a string"},
	}
	for _, tt := range tests {
		t.Run(reflect.TypeOf(tt.v).String(), func(t *testing.T) {
			err := CheckContractType(reflect.TypeOf(tt.v))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CheckContractType = %v, want a violation containing %q", err, tt.want)
			}
		})
	}
}

func TestCheckContractTypeListsEveryViolation(t *testing.T) {
	type many struct {
		A string `json:"a,omitempty"`
		B *child `json:"b"`
		C string
		d string
	}
	err := CheckContractType(reflect.TypeFor[many]())
	if err == nil {
		t.Fatal("want violations")
	}
	for _, want := range []string{"omitempty", "pointer field needs omitzero", "no explicit json name", "unexported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("violations %q do not mention %q", err, want)
		}
	}
	if n := len(strings.Split(err.Error(), "\n")); n != 4 {
		t.Errorf("got %d violations, want 4:\n%v", n, err)
	}
}

func TestLeafTypes(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[time.Time](), reflect.TypeFor[jsontext.Value](), reflect.TypeFor[leafKind](),
		reflect.TypeFor[*leafKind](), reflect.TypeFor[*time.Time](),
	} {
		if !isLeaf(typ) {
			t.Errorf("%s should be a leaf", typ)
		}
	}
	// A type that only has Validate is walked, not a leaf.
	for _, typ := range []reflect.Type{reflect.TypeFor[checkedValue](), reflect.TypeFor[child](), reflect.TypeFor[*child]()} {
		if isLeaf(typ) {
			t.Errorf("%s should not be a leaf", typ)
		}
	}
}
