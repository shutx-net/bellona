package artifact

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/strictjson"
)

type decodeCase struct {
	name    string
	input   string
	want    error // nil means the input must decode
	pointer string
}

func checkError(t *testing.T, err, want error, pointer string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	var se *strictjson.Error
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not a *strictjson.Error", err)
	}
	if se.Pointer != pointer {
		t.Errorf("pointer = %q, want %q (%v)", se.Pointer, pointer, err)
	}
}

const validTask = `{"schemaVersion":1,"request":"Add a flag"}`

func TestDecodeTask(t *testing.T) {
	tests := []decodeCase{
		{"valid", validTask, nil, ""},
		{"valid with whitespace", " \n" + validTask + "\n", nil, ""},
		{"empty input", ``, strictjson.ErrInvalidJSON, ""},
		{"invalid JSON", `{"schemaVersion":1,`, strictjson.ErrInvalidJSON, "/schemaVersion"},
		{"not an object", `[]`, strictjson.ErrInvalidValue, ""},
		{"invalid UTF-8", "{\"schemaVersion\":1,\"request\":\"a\xffb\"}", strictjson.ErrInvalidJSON, "/request"},
		{"trailing value", validTask + validTask, strictjson.ErrTrailingData, ""},
		{"trailing garbage", validTask + `x`, strictjson.ErrTrailingData, ""},
		{"duplicate key", `{"schemaVersion":1,"request":"a","request":"b"}`, strictjson.ErrDuplicateMember, "/request"},
		{"unknown field", `{"schemaVersion":1,"request":"a","extra":true}`, strictjson.ErrUnknownMember, "/extra"},
		{"wrong case field", `{"schemaVersion":1,"Request":"a"}`, strictjson.ErrUnknownMember, "/Request"},
		{"missing schemaVersion", `{"request":"a"}`, strictjson.ErrMissingMember, "/schemaVersion"},
		{"missing request", `{"schemaVersion":1}`, strictjson.ErrMissingMember, "/request"},
		{"null schemaVersion", `{"schemaVersion":null,"request":"a"}`, strictjson.ErrNull, "/schemaVersion"},
		{"null request", `{"schemaVersion":1,"request":null}`, strictjson.ErrNull, "/request"},
		{"null document", `null`, strictjson.ErrNull, ""},
		{"wrong type schemaVersion", `{"schemaVersion":"1","request":"a"}`, strictjson.ErrInvalidValue, "/schemaVersion"},
		{"wrong type request", `{"schemaVersion":1,"request":1}`, strictjson.ErrInvalidValue, "/request"},
		{"unsupported schemaVersion", `{"schemaVersion":2,"request":"a"}`, strictjson.ErrInvalidValue, "/schemaVersion"},
		{"zero schemaVersion", `{"schemaVersion":0,"request":"a"}`, strictjson.ErrInvalidValue, "/schemaVersion"},
		{"empty request", `{"schemaVersion":1,"request":""}`, strictjson.ErrInvalidValue, "/request"},
		{"blank request", `{"schemaVersion":1,"request":" \t\n"}`, strictjson.ErrInvalidValue, "/request"},
		{"oversize request", `{"schemaVersion":1,"request":"` + strings.Repeat("a", MaxRequestBytes+1) + `"}`, strictjson.ErrInvalidValue, "/request"},
		{"request at the limit", `{"schemaVersion":1,"request":"` + strings.Repeat("a", MaxRequestBytes) + `"}`, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeTask([]byte(tt.input))
			if tt.want == nil {
				if err != nil {
					t.Fatalf("DecodeTask error = %v", err)
				}
				return
			}
			checkError(t, err, tt.want, tt.pointer)
			if got != (Task{}) {
				t.Errorf("DecodeTask returned a non-zero value on error: %+v", got)
			}
		})
	}
}

func TestTaskKeepsTheRequestVerbatim(t *testing.T) {
	got, err := DecodeTask([]byte(`{"schemaVersion":1,"request":"  two\n  lines äあ  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Request != "  two\n  lines äあ  " {
		t.Errorf("Request = %q", got.Request)
	}
	if err := (Task{SchemaVersion: 1, Request: "ok"}).Validate(); err != nil {
		t.Errorf("Validate = %v", err)
	}
	if err := (Task{SchemaVersion: 1, Request: "a\xff"}).Validate(); !errors.Is(err, strictjson.ErrInvalidValue) {
		t.Errorf("Validate accepted invalid UTF-8: %v", err)
	}
	data, err := strictjson.Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeTask(data)
	if err != nil || again != got {
		t.Errorf("round trip = %+v, %v", again, err)
	}
}

const validResult = `{"schemaVersion":1,"status":"implemented","summary":"Added middleware",` +
	`"reportedChangedFiles":["internal/auth/middleware.go","internal/auth/middleware_test.go"],"issues":[]}`

func TestDecodeImplementationResult(t *testing.T) {
	long := strings.Repeat("x", MaxTextBytes+1)
	atLimit := strings.Repeat("x", MaxTextBytes)
	result := func(status, summary, files, issues string) string {
		return `{"schemaVersion":1,"status":` + status + `,"summary":` + summary + `,"reportedChangedFiles":` + files + `,"issues":` + issues + `}`
	}
	tests := []decodeCase{
		{"valid", validResult, nil, ""},
		{"valid with issues", result(`"failed"`, `"Tests fail"`, `[]`, `["TestA fails","TestB fails"]`), nil, ""},
		{"valid needs_replan", result(`"needs_replan"`, `"s"`, `[]`, `["scope too small"]`), nil, ""},
		{"valid blocked", result(`"blocked"`, `"s"`, `[]`, `[]`), nil, ""},
		{"dot files are repository paths", result(`"implemented"`, `"s"`, `[".gitignore",".github/workflows/ci.yml"]`, `[]`), nil, ""},
		{"summary at the limit", result(`"implemented"`, `"`+atLimit+`"`, `[]`, `[]`), nil, ""},
		{"empty input", ``, strictjson.ErrInvalidJSON, ""},
		{"invalid JSON", `{"schemaVersion":1,"status":`, strictjson.ErrInvalidJSON, "/status"},
		{"not JSON", `implemented`, strictjson.ErrInvalidJSON, ""},
		{"trailing value", validResult + `{}`, strictjson.ErrTrailingData, ""},
		{"trailing garbage", validResult + `]`, strictjson.ErrTrailingData, ""},
		{"duplicate key", strings.Replace(validResult, `"issues":[]`, `"issues":[],"issues":[]`, 1), strictjson.ErrDuplicateMember, "/issues"},
		{"duplicate status", strings.Replace(validResult, `"status":"implemented"`, `"status":"implemented","status":"failed"`, 1), strictjson.ErrDuplicateMember, "/status"},
		{"unknown field", strings.Replace(validResult, `"issues":[]`, `"issues":[],"confidence":0.9`, 1), strictjson.ErrUnknownMember, "/confidence"},
		{"engine-assigned field", strings.Replace(validResult, `"status"`, `"attemptId":"attempt-1","status"`, 1), strictjson.ErrUnknownMember, "/attemptId"},
		{"missing schemaVersion", `{"status":"implemented","summary":"s","reportedChangedFiles":[],"issues":[]}`, strictjson.ErrMissingMember, "/schemaVersion"},
		{"missing status", `{"schemaVersion":1,"summary":"s","reportedChangedFiles":[],"issues":[]}`, strictjson.ErrMissingMember, "/status"},
		{"missing summary", `{"schemaVersion":1,"status":"implemented","reportedChangedFiles":[],"issues":[]}`, strictjson.ErrMissingMember, "/summary"},
		{"missing reportedChangedFiles", `{"schemaVersion":1,"status":"implemented","summary":"s","issues":[]}`, strictjson.ErrMissingMember, "/reportedChangedFiles"},
		{"missing issues", `{"schemaVersion":1,"status":"implemented","summary":"s","reportedChangedFiles":[]}`, strictjson.ErrMissingMember, "/issues"},
		{"null schemaVersion", `{"schemaVersion":null,"status":"implemented","summary":"s","reportedChangedFiles":[],"issues":[]}`, strictjson.ErrNull, "/schemaVersion"},
		{"null status", result(`null`, `"s"`, `[]`, `[]`), strictjson.ErrNull, "/status"},
		{"null summary", result(`"implemented"`, `null`, `[]`, `[]`), strictjson.ErrNull, "/summary"},
		{"null reportedChangedFiles", result(`"implemented"`, `"s"`, `null`, `[]`), strictjson.ErrNull, "/reportedChangedFiles"},
		{"null issues", result(`"implemented"`, `"s"`, `[]`, `null`), strictjson.ErrNull, "/issues"},
		{"null file element", result(`"implemented"`, `"s"`, `["a.go",null]`, `[]`), strictjson.ErrNull, "/reportedChangedFiles/1"},
		{"null issue element", result(`"implemented"`, `"s"`, `[]`, `[null]`), strictjson.ErrNull, "/issues/0"},
		{"unknown status", result(`"done"`, `"s"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/status"},
		{"empty status", result(`""`, `"s"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/status"},
		{"status case", result(`"Implemented"`, `"s"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/status"},
		{"needs_user_input is not a result", result(`"needs_user_input"`, `"s"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/status"},
		{"wrong type status", result(`1`, `"s"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/status"},
		{"wrong type summary", result(`"implemented"`, `5`, `[]`, `[]`), strictjson.ErrInvalidValue, "/summary"},
		{"wrong type files", result(`"implemented"`, `"s"`, `"a.go"`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles"},
		{"wrong type file", result(`"implemented"`, `"s"`, `[1]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"wrong schemaVersion", strings.Replace(validResult, `"schemaVersion":1`, `"schemaVersion":2`, 1), strictjson.ErrInvalidValue, "/schemaVersion"},
		{"zero schemaVersion", strings.Replace(validResult, `"schemaVersion":1`, `"schemaVersion":0`, 1), strictjson.ErrInvalidValue, "/schemaVersion"},
		{"empty summary", result(`"implemented"`, `""`, `[]`, `[]`), strictjson.ErrInvalidValue, "/summary"},
		{"blank summary", result(`"implemented"`, `"  \n"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/summary"},
		{"oversize summary", result(`"implemented"`, `"`+long+`"`, `[]`, `[]`), strictjson.ErrInvalidValue, "/summary"},
		{"absolute file", result(`"implemented"`, `"s"`, `["/etc/passwd"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"dot-dot file", result(`"implemented"`, `"s"`, `["a.go","../secrets.txt"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/1"},
		{"embedded dot-dot file", result(`"implemented"`, `"s"`, `["a/../b.go"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"dot file", result(`"implemented"`, `"s"`, `["./a.go"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"empty segment file", result(`"implemented"`, `"s"`, `["a//b.go"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"empty file", result(`"implemented"`, `"s"`, `[""]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"directory-like file", result(`"implemented"`, `"s"`, `["pkg/"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"backslash file", result(`"implemented"`, `"s"`, `["pkg\\a.go"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"NUL file", result(`"implemented"`, `"s"`, `["a\u0000.go"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/0"},
		{"duplicate file", result(`"implemented"`, `"s"`, `["a.go","b.go","a.go"]`, `[]`), strictjson.ErrInvalidValue, "/reportedChangedFiles/2"},
		{"empty issue", result(`"failed"`, `"s"`, `[]`, `["ok",""]`), strictjson.ErrInvalidValue, "/issues/1"},
		{"blank issue", result(`"failed"`, `"s"`, `[]`, `[" "]`), strictjson.ErrInvalidValue, "/issues/0"},
		{"oversize issue", result(`"failed"`, `"s"`, `[]`, `["`+long+`"]`), strictjson.ErrInvalidValue, "/issues/0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeImplementationResult([]byte(tt.input))
			if tt.want == nil {
				if err != nil {
					t.Fatalf("DecodeImplementationResult error = %v", err)
				}
				return
			}
			checkError(t, err, tt.want, tt.pointer)
			if !reflect.DeepEqual(got, ImplementationResult{}) {
				t.Errorf("DecodeImplementationResult returned a non-zero value on error: %+v", got)
			}
		})
	}
}

func TestImplementationResultValue(t *testing.T) {
	got, err := DecodeImplementationResult([]byte(validResult))
	if err != nil {
		t.Fatal(err)
	}
	want := ImplementationResult{
		SchemaVersion:        1,
		Status:               StatusImplemented,
		Summary:              "Added middleware",
		ReportedChangedFiles: []string{"internal/auth/middleware.go", "internal/auth/middleware_test.go"},
		Issues:               []string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	data, err := strictjson.EncodeIndent(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeImplementationResult(data)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("round trip = %+v, %v", again, err)
	}
	// A programmatic value with nil lists encodes as empty arrays.
	nilLists := ImplementationResult{SchemaVersion: 1, Status: StatusBlocked, Summary: "s"}
	if err := nilLists.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	data, err = strictjson.Encode(nilLists)
	if err != nil || !strings.Contains(string(data), `"reportedChangedFiles":[],"issues":[]`) {
		t.Fatalf("Encode = %s, %v", data, err)
	}
	if _, err := DecodeImplementationResult(data); err != nil {
		t.Fatalf("Decode(Encode(nil lists)) = %v", err)
	}
}

func TestResultStatus(t *testing.T) {
	for _, s := range []ResultStatus{StatusImplemented, StatusNeedsReplan, StatusBlocked, StatusFailed} {
		var got ResultStatus
		b, err := s.MarshalText()
		if err != nil || got.UnmarshalText(b) != nil || got != s || !s.Valid() {
			t.Errorf("status %q: %q %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "done", "Implemented", "implemented "} {
		var s ResultStatus
		if err := s.UnmarshalText([]byte(bad)); !errors.Is(err, ErrInvalidStatus) {
			t.Errorf("UnmarshalText(%q) = %v", bad, err)
		}
		if _, err := ResultStatus(bad).MarshalText(); err == nil {
			t.Errorf("MarshalText(%q) succeeded", bad)
		}
	}
	if StatusImplemented != "implemented" || StatusNeedsReplan != "needs_replan" || StatusBlocked != "blocked" || StatusFailed != "failed" {
		t.Error("status constants")
	}
}

func TestContractTypes(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[Task](), reflect.TypeFor[ImplementationResult]()} {
		if err := strictjson.CheckContractType(typ); err != nil {
			t.Errorf("CheckContractType(%s) = %v", typ, err)
		}
	}
}
