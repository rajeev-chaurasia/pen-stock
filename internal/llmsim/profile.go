// Package llmsim is a deterministic OpenAI-wire-compatible mock LLM server.
// Its latency behavior replays a calibrated profile so load tests exercise
// realistic timing, and every response is reproducible under a seed.
package llmsim

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
)

// z95 is the standard normal quantile at 0.95.
const z95 = 1.6448536269514722

// Dist describes one metric by its target mean and 95th percentile.
type Dist struct {
	Mean float64 `json:"mean"`
	P95  float64 `json:"p95"`
}

// Profile describes the latency and output shape the server replays.
type Profile struct {
	Name         string `json:"name"`
	TTFT         Dist   `json:"ttft_ms"`
	ITL          Dist   `json:"itl_ms"`
	OutputTokens Dist   `json:"output_tokens"`
}

// DefaultProfile approximates a mid-tier hosted chat model and is used when
// no profile file is supplied.
var DefaultProfile = Profile{
	Name:         "llmsim-default",
	TTFT:         Dist{Mean: 300, P95: 900},
	ITL:          Dist{Mean: 15, P95: 40},
	OutputTokens: Dist{Mean: 180, P95: 400},
}

// params derives the lognormal mu and sigma that reproduce the requested mean
// and p95: E[X] = exp(mu + sigma^2/2) and P95 = exp(mu + z*sigma) with
// z = Phi^-1(0.95), so taking logs and subtracting gives
// sigma^2/2 - z*sigma + ln(p95/mean) = 0, whose smaller root is
// sigma = z - sqrt(z^2 - 2*ln(p95/mean)), and then mu = ln(mean) - sigma^2/2.
// When p95/mean exceeds exp(z^2/2) no exact solution exists and sigma is
// clamped to z; when p95 <= mean the distribution degenerates to the mean.
func (d Dist) params() (mu, sigma float64) {
	if d.P95 > d.Mean {
		if disc := z95*z95 - 2*math.Log(d.P95/d.Mean); disc > 0 {
			sigma = z95 - math.Sqrt(disc)
		} else {
			sigma = z95
		}
	}
	return math.Log(d.Mean) - sigma*sigma/2, sigma
}

func (d Dist) sample(rng *rand.Rand) float64 {
	if d.Mean <= 0 {
		return 0
	}
	mu, sigma := d.params()
	return math.Exp(mu + sigma*rng.NormFloat64())
}

// LoadProfile reads and validates a Profile from a JSON file.
func LoadProfile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("llmsim: read profile: %w", err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, fmt.Errorf("llmsim: parse profile %s: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return Profile{}, fmt.Errorf("llmsim: profile %s: %w", path, err)
	}
	return p, nil
}

func (p Profile) validate() error {
	fields := []struct {
		name string
		d    Dist
	}{
		{"ttft_ms", p.TTFT},
		{"itl_ms", p.ITL},
		{"output_tokens", p.OutputTokens},
	}
	for _, f := range fields {
		if f.d.Mean <= 0 || f.d.P95 <= 0 {
			return fmt.Errorf("%s: mean and p95 must be positive", f.name)
		}
		if f.d.P95 < f.d.Mean {
			return fmt.Errorf("%s: p95 must be at least the mean", f.name)
		}
	}
	return nil
}
