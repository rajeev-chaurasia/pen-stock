package llmsim

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func relDiff(got, want float64) float64 {
	return math.Abs(got-want) / want
}

func TestDistParamsMatchMeanAndP95(t *testing.T) {
	cases := []struct {
		name string
		d    Dist
	}{
		{"default ttft", Dist{Mean: 300, P95: 900}},
		{"default itl", Dist{Mean: 15, P95: 40}},
		{"default tokens", Dist{Mean: 180, P95: 400}},
		{"tight spread", Dist{Mean: 100, P95: 101}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu, sigma := tc.d.params()
			if sigma <= 0 {
				t.Fatalf("sigma = %v, want > 0", sigma)
			}
			if got := math.Exp(mu + sigma*sigma/2); relDiff(got, tc.d.Mean) > 1e-9 {
				t.Errorf("implied mean = %v, want %v", got, tc.d.Mean)
			}
			if got := math.Exp(mu + z95*sigma); relDiff(got, tc.d.P95) > 1e-9 {
				t.Errorf("implied p95 = %v, want %v", got, tc.d.P95)
			}
		})
	}
}

func TestDistParamsDegenerate(t *testing.T) {
	mu, sigma := Dist{Mean: 100, P95: 100}.params()
	if sigma != 0 {
		t.Errorf("sigma = %v, want 0 when p95 equals mean", sigma)
	}
	if got := math.Exp(mu); relDiff(got, 100) > 1e-9 {
		t.Errorf("implied constant = %v, want 100", got)
	}
}

func TestLoadProfile(t *testing.T) {
	valid := `{"name":"fast","ttft_ms":{"mean":100,"p95":200},"itl_ms":{"mean":5,"p95":9},"output_tokens":{"mean":50,"p95":90}}`
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"valid", valid, false},
		{"malformed json", "{", true},
		{"zero mean", `{"name":"x","ttft_ms":{"mean":0,"p95":1},"itl_ms":{"mean":1,"p95":1},"output_tokens":{"mean":1,"p95":1}}`, true},
		{"p95 below mean", `{"name":"x","ttft_ms":{"mean":10,"p95":5},"itl_ms":{"mean":1,"p95":1},"output_tokens":{"mean":1,"p95":1}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			p, err := LoadProfile(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadProfile: %v", err)
			}
			if p.Name != "fast" || p.TTFT.Mean != 100 || p.ITL.P95 != 9 || p.OutputTokens.Mean != 50 {
				t.Errorf("unexpected profile: %+v", p)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadProfile(filepath.Join(t.TempDir(), "nope.json")); err == nil {
			t.Fatal("expected an error")
		}
	})
}
