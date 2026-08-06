# 0011. The semantic cache tier is opt in, with a similarity floor of 0.95

## Status

Accepted.

## Context

A semantic cache answers a request that is *similar* to one already seen,
rather than identical to it. It is the headline feature of most LLM
gateways, it demos well, and the published numbers for it are hit rates.

The hit rate is not the number that matters. The number that matters is
the false hit rate, which is how often the cache returns an answer to a
different question than the one asked, and it is the number nobody
publishes.

Penstock's semantic tier was built, wired to a live embedder, and
measured before the threshold was chosen. The measurement is what this
decision is made of. It is reproducible: `TestMeasureRealSimilarities` in
[internal/cache](../../internal/cache/threshold_probe_test.go) skips
unless a `GEMINI_API_KEY` is present, so no build depends on a live
service.

Embedding real questions and comparing them by cosine similarity
produced this:

| Cosine | Relationship | Pair |
|---|---|---|
| 0.829 | paraphrase | "What is the capital city of France?" vs "Which city is the capital of France?" |
| 0.810 | paraphrase | "How do I sort a list in Python?" vs "What is the way to sort a python list?" |
| 0.803 | **opposite** | "How do I start the service?" vs "How do I stop the service?" |
| 0.908 | **opposite** | "How do I enable logging in this application?" vs "How do I disable logging in this application?" |
| 0.926 | **opposite** | "Is it safe to delete this file?" vs "Is it unsafe to delete this file?" |
| 0.942 | **opposite** | "Should I use a mutex here?" vs "Should I avoid a mutex here?" |
| 0.513 | unrelated | "What is the capital of France?" vs "What is the boiling point of water?" |

The two groups do not merely overlap. Three of the four opposite meaning
pairs score higher than either genuine paraphrase, and the highest score
in the whole table belongs to a pair that means the reverse of itself.

That is not a quirk of these sentences. Negating a question changes one
short word, which moves a sentence embedding very little. Rephrasing a
question changes most of the words, which moves it a lot. Cosine distance
is measuring how many words changed, which is the wrong quantity for
deciding whether two questions are the same question. No threshold
separates the groups, because the groups are in the wrong order.

The consequence was observed rather than theorized. At a threshold of
0.90, tuned to catch the paraphrases above, this gateway served the
answer to "how do I enable logging" when asked "how do I disable
logging".

## Decision

The exact tier is the default. Enabling caching gets exact matching
only, after canonicalization so that JSON key order and insignificant
whitespace cannot cause a spurious miss. That is where the savings come
from and it cannot return a wrong answer.

The semantic tier is opt in, under its own `cache.semantic.enabled`
flag.

The configured threshold has a floor of 0.95.
`config.MinSemanticThreshold` is 0.95 and the loader rejects any
configured value below it or above 1
([internal/config/config.go](../../internal/config/config.go)). An
unset threshold is permitted and resolves to
`cache.DefaultSimilarityThreshold`, which is also 0.95
([internal/cache/semantic.go](../../internal/cache/semantic.go)), so
there is no path to a looser tier through omission.

The floor is set above the highest opposite pair measured, 0.942, rather
than below the lowest paraphrase, 0.810. That ordering is the decision.
It means the tier fires rarely, and rarely is the correct frequency when
the failure mode is answering the opposite of what was asked.

Turning it on logs a warning at startup naming the risk, with the
threshold and the embedding model in the fields
([cmd/penstock/main.go](../../cmd/penstock/main.go)). An operator
enabling this should have decided that a wrong answer is cheaper than an
API call for their workload, rather than discovering it later.

Cache outcomes are separate metric events, so `semantic_hit` can be
watched independently of `exact_hit` and a rising count is visible rather
than buried in an aggregate hit rate.

The measurement ships as a test rather than as a paragraph, so the claim
can be rechecked against a newer embedding model instead of taken on
trust.

The reasoning is written up for users in
[docs/semantic-caching.md](../semantic-caching.md) and restated in the
[internal/cache](../../internal/cache/policy.go) package doc.

## Consequences

The semantic tier is nearly useless at 0.95, which is the intended
outcome. A gateway that ships it enabled with a 0.85 threshold would show
a better hit rate in a benchmark and would be wrong more often, and
nobody would be able to tell from the outside.

Some workloads genuinely tolerate a looser tier: a support bot answering
FAQ variants where every plausible neighbour has the same answer, or a
classification prompt with a small fixed output space. Those workloads
are not served well here, because the floor is set for the general case.
The tier exists for them, at 0.95, which is stricter than they need.

Enabling the tier costs an embedding API call on the request path before
the cache can be consulted at all, so a miss is slower than it would be
with the exact tier alone. An embedder that is down costs a cache hit and
never a request: the semantic tier degrades to the exact one rather than
failing traffic.

The measurement is a snapshot of one embedding model. A better model
might separate the groups, and nothing here would notice. That is why the
test exists and why it is named as the thing to re-run.

What would make the tier safe more generally is a verification step,
sending the candidate hit and the new question to a small model to
confirm they are the same question. That costs an API call, which is the
thing the cache exists to avoid, so it is only worth it when the cached
answer is expensive. That work is not built.

## Alternatives considered

**Ship the semantic tier on by default with a threshold around 0.85 to
0.90, as is common.** Rejected by measurement. 0.90 is below three of
the four opposite pairs, and it is the exact configuration under which
this gateway served the enable-logging answer to a disable-logging
question. A cache that is fast and sometimes answers the opposite
question is not a faster gateway, it is a broken one.

**Pick the threshold between the paraphrase group and the opposite
group.** Rejected because there is no such gap. The opposite group sits
above the paraphrase group, so any threshold low enough to catch a
paraphrase catches three negations first.

**A better embedding model, or an asymmetric or instruction tuned
embedder.** Not rejected on principle, just not verified. Nothing was
found that separated the groups, and shipping a default that depends on
an unverified claim about an embedder is the failure this ADR exists to
avoid. The probe test is the mechanism for revisiting it.

**A cross encoder or a reranker over candidate hits.** Rejected for now
on cost. It is the technically right answer and it is the "verification
step" named above. It requires a model call per candidate hit, which
undoes the cache's purpose unless the cached answer is expensive enough
to justify it. Worth building when there is a workload that justifies it.

**Drop the semantic tier entirely.** Seriously considered and rejected.
The exact tier is where the savings are, so removing it would cost
almost nothing. It stays because some workloads genuinely tolerate it,
and because the measurement is more useful shipped alongside a working
implementation than as an assertion that the feature is impossible.

**Report only an aggregate cache hit rate.** Rejected. It hides exactly
the number an operator needs to judge whether their threshold is set
right, which is how often the answer came from a different question.
