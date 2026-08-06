package cache

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const (
	testTenant = "acme"
	testModel  = "gpt-4o-mini"

	// baseRequest is the request every case in this file varies from.
	baseRequest = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0}`
)

func mustBuildKey(t *testing.T, tenant, model, body string) Key {
	t.Helper()
	k, err := BuildKey(tenant, model, []byte(body))
	if err != nil {
		t.Fatalf("BuildKey(%s): unexpected error: %v", body, err)
	}
	return k
}

// requireDistinctHashes fails unless every body hashes to its own value.
func requireDistinctHashes(t *testing.T, bodies map[string]string) {
	t.Helper()
	seen := make(map[string]string, len(bodies))
	for name, body := range bodies {
		k := mustBuildKey(t, testTenant, testModel, body)
		if prior, clash := seen[k.Hash]; clash {
			t.Errorf("%q and %q share hash %s, so one would be served the other's answer", prior, name, k.Hash)
			continue
		}
		seen[k.Hash] = name
	}
}

// requireSameHash fails unless every body hashes to the base request's
// value.
func requireSameHash(t *testing.T, bodies map[string]string) {
	t.Helper()
	want := mustBuildKey(t, testTenant, testModel, baseRequest).Hash
	for name, body := range bodies {
		got := mustBuildKey(t, testTenant, testModel, body).Hash
		if got != want {
			t.Errorf("%q hashed to %s, want %s: the same question missed the cache", name, got, want)
		}
	}
}

func TestBuildKeyIgnoresKeyOrderAndWhitespace(t *testing.T) {
	requireSameHash(t, map[string]string{
		"top level keys reordered": `{"temperature":0,"messages":[{"role":"user","content":"hello"}],"model":"gpt-4o-mini"}`,
		"nested keys reordered":    `{"model":"gpt-4o-mini","messages":[{"content":"hello","role":"user"}],"temperature":0}`,
		"indented and newline separated": `{
			"model" : "gpt-4o-mini" ,
			"temperature" : 0 ,
			"messages" : [
				{ "content" : "hello" , "role" : "user" }
			]
		}`,
		"leading and trailing whitespace": "\n\t  " + baseRequest + "  \n",
	})
}

func TestBuildKeyCanonicalizesNestedObjectKeyOrder(t *testing.T) {
	withFormat := func(inner string) string {
		return `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"response_format":` + inner + `}`
	}
	first := mustBuildKey(t, testTenant, testModel,
		withFormat(`{"type":"json_schema","json_schema":{"name":"reply","strict":true}}`))
	second := mustBuildKey(t, testTenant, testModel,
		withFormat(`{"json_schema":{"strict":true,"name":"reply"},"type":"json_schema"}`))

	if first.Hash != second.Hash {
		t.Errorf("nested key order changed the hash: %s vs %s", first.Hash, second.Hash)
	}
}

// TestBuildKeyKeepsNumberLiterals pins the safe side of a deliberate
// trade. Numbers are not routed through a float during canonicalization,
// so no two distinct values can round together into one key. The price
// is that 1 and 1.0 are treated as different requests, which costs a
// miss and never a wrong answer.
func TestBuildKeyKeepsNumberLiterals(t *testing.T) {
	integer := mustBuildKey(t, testTenant, testModel,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":1}`)
	decimal := mustBuildKey(t, testTenant, testModel,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":1.0}`)

	if integer.Hash == decimal.Hash {
		t.Errorf("number literals were normalized through a float, which can round two values together: %s", integer.Hash)
	}
}

func TestBuildKeyIsStableAcrossCalls(t *testing.T) {
	first := mustBuildKey(t, testTenant, testModel, baseRequest)
	second := mustBuildKey(t, testTenant, testModel, baseRequest)

	if first != second {
		t.Errorf("the same request built two keys: %+v vs %+v", first, second)
	}
}

func TestBuildKeyDistinguishesMessages(t *testing.T) {
	requireDistinctHashes(t, map[string]string{
		"base":                  baseRequest,
		"different content":     `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"goodbye"}],"temperature":0}`,
		"different role":        `{"model":"gpt-4o-mini","messages":[{"role":"system","content":"hello"}],"temperature":0}`,
		"extra message":         `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}],"temperature":0}`,
		"messages reordered":    `{"model":"gpt-4o-mini","messages":[{"role":"assistant","content":"hi"},{"role":"user","content":"hello"}],"temperature":0}`,
		"content as parts":      `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"temperature":0}`,
		"parts reordered":       `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"world"},{"type":"text","text":"hello"}]}],"temperature":0}`,
		"parts in order":        `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}],"temperature":0}`,
		"part carries an image": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}],"temperature":0}`,
		"no messages":           `{"model":"gpt-4o-mini","messages":[],"temperature":0}`,
	})
}

// TestBuildKeySamplingFieldsChangeTheHash covers every field named in
// the cache key contract plus fields the gateway does not model, which
// are hashed too so that an unmodeled knob costs a miss rather than a
// wrong answer.
func TestBuildKeySamplingFieldsChangeTheHash(t *testing.T) {
	requireDistinctHashes(t, map[string]string{
		"base":                                baseRequest,
		"temperature raised":                  `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0.7}`,
		"top_p set":                           `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"top_p":0.9}`,
		"top_p lowered":                       `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"top_p":0.8}`,
		"max_tokens set":                      `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"max_tokens":256}`,
		"max_tokens raised":                   `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"max_tokens":512}`,
		"max_completion_tokens set":           `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"max_completion_tokens":256}`,
		"stop set":                            `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stop":["\n"]}`,
		"stop as a bare string":               `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stop":"\n"}`,
		"stop with another sequence":          `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stop":["END"]}`,
		"presence_penalty set":                `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"presence_penalty":0.5}`,
		"frequency_penalty set":               `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"frequency_penalty":0.5}`,
		"response_format set":                 `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"response_format":{"type":"json_object"}}`,
		"response_format with a schema":       `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"response_format":{"type":"json_schema"}}`,
		"seed set":                            `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"seed":7}`,
		"n set":                               `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"n":2}`,
		"logprobs set":                        `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"logprobs":true}`,
		"logit_bias set":                      `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"logit_bias":{"50256":-100}}`,
		"a field the gateway does not model":  `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"reasoning_effort":"high"}`,
		"the unmodeled field set differently": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"reasoning_effort":"low"}`,
	})
}

func TestBuildKeyIgnoresFieldsThatDoNotChangeTheAnswer(t *testing.T) {
	requireSameHash(t, map[string]string{
		"stream true":  `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":true}`,
		"stream false": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":false}`,
		"stream_options": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,` +
			`"stream":true,"stream_options":{"include_usage":true}}`,
		"user":     `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"user":"employee-17"}`,
		"metadata": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"metadata":{"trace":"abc"}}`,
		"all of them at once": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,` +
			`"stream":true,"stream_options":{"include_usage":true},"user":"employee-17","metadata":{"trace":"abc"}}`,
	})
}

// TestBuildKeyStreamingAndNonStreamingShareAKey states the property the
// entry shape depends on: one stored answer serves both callers.
func TestBuildKeyStreamingAndNonStreamingShareAKey(t *testing.T) {
	streaming := mustBuildKey(t, testTenant, testModel,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":true}`)
	blocking := mustBuildKey(t, testTenant, testModel,
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":false}`)

	if streaming.Hash != blocking.Hash {
		t.Errorf("streaming and non streaming forms of one question hashed apart: %s vs %s", streaming.Hash, blocking.Hash)
	}
}

// TestBuildKeyOnlyPrunesTheTopLevel guards the rule that an excluded
// name nested inside the conversation is still part of the question. A
// recursive prune would drop content the model actually reads.
func TestBuildKeyOnlyPrunesTheTopLevel(t *testing.T) {
	requireDistinctHashes(t, map[string]string{
		"base": baseRequest,
		"metadata inside a content part": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":` +
			`[{"type":"text","text":"hello","metadata":{"note":"read this"}}]}],"temperature":0}`,
		"user inside a message": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello","user":"named"}],"temperature":0}`,
		"stream inside a message": `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello","stream":true}],` +
			`"temperature":0}`,
	})
}

// TestBuildKeyKeepsTenantOutOfTheHash proves isolation is enforced by
// where an entry is stored rather than by the digest.
func TestBuildKeyKeepsTenantOutOfTheHash(t *testing.T) {
	acme := mustBuildKey(t, "acme", testModel, baseRequest)
	globex := mustBuildKey(t, "globex", testModel, baseRequest)

	if acme.Hash != globex.Hash {
		t.Errorf("tenant leaked into the hash: %s vs %s", acme.Hash, globex.Hash)
	}
	if acme.Tenant != "acme" || globex.Tenant != "globex" {
		t.Errorf("tenant not carried on the key: %q and %q", acme.Tenant, globex.Tenant)
	}
}

// TestBuildKeyRoutedModelChangesTheHash covers the model resolved by the
// router, which can differ from the one named in the body.
func TestBuildKeyRoutedModelChangesTheHash(t *testing.T) {
	mini := mustBuildKey(t, testTenant, "gpt-4o-mini", baseRequest)
	full := mustBuildKey(t, testTenant, "gpt-4o", baseRequest)

	if mini.Hash == full.Hash {
		t.Errorf("two models shared hash %s, so one model would serve the other's answer", mini.Hash)
	}
	if mini.Model != "gpt-4o-mini" || full.Model != "gpt-4o" {
		t.Errorf("model not carried on the key: %q and %q", mini.Model, full.Model)
	}
}

func TestBuildKeyHashIsHexSHA256(t *testing.T) {
	k := mustBuildKey(t, testTenant, testModel, baseRequest)

	if len(k.Hash) != 64 {
		t.Errorf("hash is %d characters, want 64 for hex SHA-256: %q", len(k.Hash), k.Hash)
	}
	if _, err := hex.DecodeString(k.Hash); err != nil {
		t.Errorf("hash is not hex: %v", err)
	}
	if strings.ToLower(k.Hash) != k.Hash {
		t.Errorf("hash is not lower case hex: %q", k.Hash)
	}
}

func TestBuildKeyRejectsUnusableBodies(t *testing.T) {
	cases := map[string]struct {
		body string
		want error
	}{
		"empty body":          {body: ``},
		"truncated object":    {body: `{"model":"gpt-4o-mini"`},
		"not json at all":     {body: `this is not json`},
		"unquoted key":        {body: `{model: "gpt-4o-mini"}`},
		"array at top level":  {body: `[{"role":"user"}]`, want: ErrNotJSONObject},
		"string at top level": {body: `"hello"`, want: ErrNotJSONObject},
		"number at top level": {body: `42`, want: ErrNotJSONObject},
		"null at top level":   {body: `null`, want: ErrNotJSONObject},
		"two objects":         {body: `{"a":1} {"b":2}`, want: ErrTrailingData},
		"trailing garbage":    {body: `{"a":1} nonsense`, want: ErrTrailingData},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			k, err := BuildKey(testTenant, testModel, []byte(tc.body))
			if err == nil {
				t.Fatalf("BuildKey accepted %q and produced %+v", tc.body, k)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("got error %v, want %v", err, tc.want)
			}
			if k != (Key{}) {
				t.Errorf("a failed BuildKey returned a usable key: %+v", k)
			}
		})
	}
}

// TestBuildKeyDoesNotModifyTheCallersBody matters because the same bytes
// are forwarded upstream after the key is built.
func TestBuildKeyDoesNotModifyTheCallersBody(t *testing.T) {
	raw := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":true}`)
	before := string(raw)

	if _, err := BuildKey(testTenant, testModel, raw); err != nil {
		t.Fatalf("BuildKey: %v", err)
	}
	if string(raw) != before {
		t.Errorf("BuildKey rewrote the request body:\n got %s\nwant %s", raw, before)
	}
}
