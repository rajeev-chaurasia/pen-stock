package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// corpusSchema is the only corpus shape this command reads. It is
// checked rather than assumed, so a file written for a later schema is
// refused instead of being half understood.
const corpusSchema = "penstock/cache-quality/corpus/v1"

// Label names the relationship between a probe and its group's anchor.
// The label is the ground truth: it is what makes a hit judgeable
// without asking a model whether two questions mean the same thing,
// which is the thing under test.
type Label string

const (
	// LabelRepeat is the anchor sent again, byte for byte. A hit is
	// correct and is what the exact tier exists for.
	LabelRepeat Label = "repeat"
	// LabelParaphrase is the same question in different words, written so
	// that the anchor's answer is genuinely interchangeable with its own.
	// A hit is correct.
	LabelParaphrase Label = "paraphrase"
	// LabelOpposite is the anchor with one word flipped so that it asks
	// the opposite thing. A hit is a false hit, and its rate is the
	// number this study exists to publish.
	LabelOpposite Label = "opposite"
	// LabelUnrelated is a question from another subject entirely. It is
	// the floor: a cache that hits here is not similar-but-wrong, it is
	// broken.
	LabelUnrelated Label = "unrelated"
)

// labelOrder fixes the order labels appear in every table and result
// file, so two runs diff cleanly.
var labelOrder = []Label{LabelRepeat, LabelParaphrase, LabelOpposite, LabelUnrelated}

var knownLabels = map[Label]struct{}{
	LabelRepeat:     {},
	LabelParaphrase: {},
	LabelOpposite:   {},
	LabelUnrelated:  {},
}

// ErrCorpus is the root of every corpus loading failure.
var ErrCorpus = errors.New("corpus")

// Corpus is a set of question groups whose labels are known by
// construction.
type Corpus struct {
	Schema string  `json:"schema"`
	Note   string  `json:"note"`
	Groups []Group `json:"groups"`
}

// Group is one question and the probes that relate to it. The anchor is
// what a warm cache holds; the probes are the traffic that arrives
// afterwards.
type Group struct {
	ID     string  `json:"id"`
	Domain string  `json:"domain"`
	Anchor string  `json:"anchor"`
	Probes []Probe `json:"probes"`
}

// Probe is one labelled request against a group's anchor.
type Probe struct {
	Label Label  `json:"label"`
	Text  string `json:"text"`
	// Flip records which word was changed to invert an opposite. It
	// documents the construction so a reader can check the label rather
	// than trust it.
	Flip string `json:"flip,omitempty"`
}

// LoadCorpus reads and validates a corpus file.
func LoadCorpus(path string) (*Corpus, error) {
	// The path comes from the operator's own command line, so there is
	// no untrusted input to traverse with.
	// #nosec G304
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrCorpus, path, err)
	}
	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrCorpus, path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCorpus, path, err)
	}
	return &c, nil
}

// validate refuses a corpus whose labels cannot be trusted.
//
// Every rule here protects a claim the study makes later. A repeat that
// is not byte identical to its anchor would not be a repeat, so the
// exact tier's 100 percent would be measuring something else. A text
// reused across groups would have two correct answers, so a hit on it
// could not be scored. A group with no opposite contributes nothing to
// the only number that matters.
func (c *Corpus) validate() error {
	if c.Schema != corpusSchema {
		return fmt.Errorf("schema is %q, want %q", c.Schema, corpusSchema)
	}
	if len(c.Groups) == 0 {
		return errors.New("no groups")
	}

	seenID := make(map[string]struct{}, len(c.Groups))
	// Every question in the corpus, anchors and probes alike, has to be
	// unique across the whole file. Two groups sharing a text would make
	// one lookup answerable two ways.
	seenText := make(map[string]string)

	for i := range c.Groups {
		g := &c.Groups[i]
		if g.ID == "" {
			return fmt.Errorf("group %d has no id", i)
		}
		if _, dup := seenID[g.ID]; dup {
			return fmt.Errorf("group id %q appears twice", g.ID)
		}
		seenID[g.ID] = struct{}{}
		if g.Domain == "" {
			return fmt.Errorf("group %q has no domain", g.ID)
		}
		if g.Anchor == "" {
			return fmt.Errorf("group %q has no anchor", g.ID)
		}
		if owner, dup := seenText[g.Anchor]; dup {
			return fmt.Errorf("group %q anchor is already used by %q", g.ID, owner)
		}
		seenText[g.Anchor] = g.ID

		if err := g.validateProbes(seenText); err != nil {
			return fmt.Errorf("group %q: %w", g.ID, err)
		}
	}
	return nil
}

// validateProbes checks one group's probes and records their texts in
// the corpus wide uniqueness set.
func (g *Group) validateProbes(seenText map[string]string) error {
	counts := make(map[Label]int, len(knownLabels))
	for i := range g.Probes {
		p := &g.Probes[i]
		if _, ok := knownLabels[p.Label]; !ok {
			return fmt.Errorf("probe %d has unknown label %q", i, p.Label)
		}
		if p.Text == "" {
			return fmt.Errorf("probe %d has no text", i)
		}
		counts[p.Label]++

		if p.Label == LabelRepeat {
			// A repeat is the anchor resent. Anything else is a
			// paraphrase wearing the wrong label.
			if p.Text != g.Anchor {
				return fmt.Errorf("repeat probe is not byte identical to the anchor")
			}
			continue
		}
		if owner, dup := seenText[p.Text]; dup {
			return fmt.Errorf("probe %d text is already used by %q", i, owner)
		}
		seenText[p.Text] = g.ID
		if p.Label == LabelOpposite && p.Flip == "" {
			return fmt.Errorf("opposite probe %d does not record which word was flipped", i)
		}
	}

	if counts[LabelRepeat] != 1 {
		return fmt.Errorf("has %d repeat probes, want exactly 1", counts[LabelRepeat])
	}
	if counts[LabelParaphrase] == 0 {
		return errors.New("has no paraphrase probe")
	}
	if counts[LabelOpposite] == 0 {
		return errors.New("has no opposite probe")
	}
	if counts[LabelUnrelated] == 0 {
		return errors.New("has no unrelated probe")
	}
	return nil
}

// LabelCounts reports how many probes carry each label.
func (c *Corpus) LabelCounts() map[Label]int {
	counts := make(map[Label]int, len(knownLabels))
	for i := range c.Groups {
		for _, p := range c.Groups[i].Probes {
			counts[p.Label]++
		}
	}
	return counts
}

// DomainCounts reports how many groups each domain contributes.
func (c *Corpus) DomainCounts() map[string]int {
	counts := make(map[string]int)
	for i := range c.Groups {
		counts[c.Groups[i].Domain]++
	}
	return counts
}
