package oauth

import (
	"reflect"
	"testing"
)

func TestParseChallenges(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []challenge
	}{
		{"empty", "", nil},
		{"bare scheme", "Bearer", []challenge{{scheme: "bearer", params: map[string]string{}}}},
		{"quoted", `Bearer resource_metadata="https://a.example/x"`, []challenge{
			{scheme: "bearer", params: map[string]string{"resource_metadata": "https://a.example/x"}},
		}},
		{"case and spacing", `BEARER  Realm = "r" ,Error=invalid_token`, []challenge{
			{scheme: "bearer", params: map[string]string{"realm": "r", "error": "invalid_token"}},
		}},
		{"escapes and commas in quotes", `Bearer realm="a \"b\", c\\d", scope="x y"`, []challenge{
			{scheme: "bearer", params: map[string]string{"realm": `a "b", c\d`, "scope": "x y"}},
		}},
		{"several schemes", `Basic realm="b", Bearer resource_metadata="https://m", Negotiate abc+/==`, []challenge{
			{scheme: "basic", params: map[string]string{"realm": "b"}},
			{scheme: "bearer", params: map[string]string{"resource_metadata": "https://m"}},
			{scheme: "negotiate", token68: "abc+/==", params: map[string]string{}},
		}},
		{"token68 then scheme", `Negotiate abc, Bearer`, []challenge{
			{scheme: "negotiate", token68: "abc", params: map[string]string{}},
			{scheme: "bearer", params: map[string]string{}},
		}},
		{"first value wins", `Bearer a=1, A=2`, []challenge{
			{scheme: "bearer", params: map[string]string{"a": "1"}},
		}},
		{"unterminated quote", `Bearer a="open`, []challenge{
			{scheme: "bearer", params: map[string]string{"a": "open"}},
		}},
		{"garbage stops", `Bearer a=1, ="x" Basic`, []challenge{
			{scheme: "bearer", params: map[string]string{"a": "1"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseChallenges(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseChallenges(%q)\n got %#v\nwant %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBearerParam(t *testing.T) {
	h := `Basic resource_metadata="https://wrong", bearer resource_metadata="https://right"`
	if got := bearerParam(h, "resource_metadata"); got != "https://right" {
		t.Errorf("got %q", got)
	}
	if got := bearerParam(`Basic realm="x"`, "resource_metadata"); got != "" {
		t.Errorf("no bearer: got %q", got)
	}
}
