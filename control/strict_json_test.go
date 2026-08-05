package main

import (
	"strings"
	"testing"
)

type strictJSONNested struct {
	Name string `json:"name"`
}

type strictJSONFixture struct {
	FundRef string             `json:"fund_ref"`
	Cents   int64              `json:"cents"`
	Nested  strictJSONNested   `json:"nested"`
	Items   []strictJSONNested `json:"items"`
}

func TestDecodeStrictJSONObjectAcceptsOneTypedObject(t *testing.T) {
	raw := []byte(`
	 {
	   "fund_ref": "fund-1",
	   "cents": 123,
	   "nested": {"name": "inside"},
	   "items": [{"name": "first"}, {"name": "second"}]
	 }
	`)
	var got strictJSONFixture
	mustf(t, decodeStrictJSONObject(raw, &got), "decodeStrictJSONObject: %v")
	if got.FundRef != "fund-1" || got.Cents != 123 || got.Nested.Name != "inside" ||
		len(got.Items) != 2 || got.Items[0].Name != "first" || got.Items[1].Name != "second" {
		t.Fatalf("decoded value = %+v", got)
	}
}

func TestDecodeStrictJSONObjectRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		key  string
	}{
		{"top level", `{"fund_ref":"one","fund_ref":"two"}`, "fund_ref"},
		{"empty key", `{"":1,"":2}`, ""},
		{"escaped equivalent", `{"fund_ref":"one","fund_\u0072ef":"two"}`, "fund_ref"},
		{"nested object", `{"nested":{"name":"one","name":"two"}}`, "name"},
		{"object in array", `{"items":[{"name":"one","name":"two"}]}`, "name"},
		{"deep mixed containers", `{"items":[{"name":"ok"},{"child":{"x":1,"x":2}}]}`, "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst map[string]any
			err := decodeStrictJSONObject([]byte(tc.raw), &dst)
			if err == nil {
				t.Fatal("duplicate key was accepted")
			}
			if !strings.Contains(err.Error(), "duplicate key") || !strings.Contains(err.Error(), `"`+tc.key+`"`) {
				t.Fatalf("error = %q, want duplicate key %q", err, tc.key)
			}
		})
	}
}

func TestDecodeStrictJSONObjectAllowsSameKeyInSeparateObjects(t *testing.T) {
	var got struct {
		Items []strictJSONNested `json:"items"`
	}
	if err := decodeStrictJSONObject(
		[]byte(`{"items":[{"name":"one"},{"name":"two"}]}`), &got,
	); err != nil {
		t.Fatalf("keys in separate object scopes were rejected: %v", err)
	}
	if len(got.Items) != 2 || got.Items[0].Name != "one" || got.Items[1].Name != "two" {
		t.Fatalf("decoded value = %+v", got)
	}
}

func TestDecodeStrictJSONObjectRejectsMalformedAndTrailingInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ``},
		{"whitespace only", " \n\t"},
		{"missing value", `{"fund_ref":}`},
		{"missing object close", `{"fund_ref":"one"`},
		{"missing array close", `{"items":[{"name":"one"}]`},
		{"trailing comma", `{"fund_ref":"one",}`},
		{"invalid separator", `{"fund_ref":"one";"cents":1}`},
		{"two objects", `{"fund_ref":"one"} {"fund_ref":"two"}`},
		{"object then scalar", `{"fund_ref":"one"} true`},
		{"trailing garbage", `{"fund_ref":"one"} xyz`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst map[string]any
			if err := decodeStrictJSONObject([]byte(tc.raw), &dst); err == nil {
				t.Fatalf("invalid input %q was accepted", tc.raw)
			}
		})
	}
}

func TestDecodeStrictJSONObjectRequiresTopLevelObject(t *testing.T) {
	for _, raw := range []string{
		`[]`, `[{}]`, `null`, `true`, `false`, `1`, `"text"`,
	} {
		t.Run(raw, func(t *testing.T) {
			var dst any
			err := decodeStrictJSONObject([]byte(raw), &dst)
			if err == nil || !strings.Contains(err.Error(), "top-level value must be an object") {
				t.Fatalf("error = %v, want top-level object refusal", err)
			}
		})
	}
}

func TestDecodeStrictJSONObjectDisallowsUnknownTypedFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"top-level unknown", `{"fund_ref":"one","unexpected":true}`, "unexpected"},
		{"nested unknown", `{"nested":{"name":"one","unexpected":true}}`, "unexpected"},
		{"array element unknown", `{"items":[{"name":"one","unexpected":true}]}`, "unexpected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst strictJSONFixture
			err := decodeStrictJSONObject([]byte(tc.raw), &dst)
			if err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want unknown field %q", err, tc.want)
			}
		})
	}
}

func TestDecodeStrictJSONObjectChecksTypesAfterStructuralValidation(t *testing.T) {
	var dst strictJSONFixture
	err := decodeStrictJSONObject([]byte(`{"fund_ref":"one","cents":"not-a-number"}`), &dst)
	if err == nil || !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("error = %v, want typed decode error", err)
	}
}

func TestDecodeStrictJSONObjectDoesNotMutateDestinationOnPreflightFailure(t *testing.T) {
	dst := strictJSONFixture{FundRef: "unchanged", Cents: 77}
	err := decodeStrictJSONObject([]byte(`{"fund_ref":"first","fund_ref":"second"}`), &dst)
	if err == nil {
		t.Fatal("duplicate input was accepted")
	}
	if dst.FundRef != "unchanged" || dst.Cents != 77 {
		t.Fatalf("destination changed before structural validation: %+v", dst)
	}
}

func TestDecodeStrictJSONObjectRejectsNilDestination(t *testing.T) {
	err := decodeStrictJSONObject([]byte(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "destination is nil") {
		t.Fatalf("error = %v, want nil-destination refusal", err)
	}
}

func TestRejectDuplicateJSONKeysCanValidateWithoutTypedDecode(t *testing.T) {
	mustf(t, rejectDuplicateJSONKeys([]byte(`{"arbitrary":{"shape":[1,true,null,"ok"]}}`)), "valid arbitrary object rejected: %v")
	if err := rejectDuplicateJSONKeys([]byte(`{"arbitrary":{"x":1,"x":2}}`)); err == nil {
		t.Fatal("nested duplicate accepted")
	}
}
