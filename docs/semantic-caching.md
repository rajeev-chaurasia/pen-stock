# Semantic caching, and why it is off by default

Penstock has two cache tiers. The exact tier answers a request that is
identical to one it has already seen, after canonicalizing the request
so key order and whitespace cannot cause a spurious miss. It is safe,
it is on when caching is enabled, and it is where the savings come
from.

The semantic tier answers a request that is *similar* to one already
seen. It is off by default. This page explains why, with the numbers.

## The measurement

Embedding questions with a current model and comparing them by cosine
similarity produces this. The test that reproduces it is
`TestMeasureRealSimilarities` in `internal/cache`, which skips unless a
`GEMINI_API_KEY` is present so no build depends on a live service.

| Cosine | Relationship | Pair |
|---|---|---|
| 0.829 | paraphrase | "What is the capital city of France?" vs "Which city is the capital of France?" |
| 0.810 | paraphrase | "How do I sort a list in Python?" vs "What is the way to sort a python list?" |
| 0.803 | **opposite** | "How do I start the service?" vs "How do I stop the service?" |
| 0.908 | **opposite** | "How do I enable logging in this application?" vs "How do I disable logging in this application?" |
| 0.926 | **opposite** | "Is it safe to delete this file?" vs "Is it unsafe to delete this file?" |
| 0.942 | **opposite** | "Should I use a mutex here?" vs "Should I avoid a mutex here?" |
| 0.513 | unrelated | "What is the capital of France?" vs "What is the boiling point of water?" |

## What the numbers say

The two groups do not merely overlap. They invert. Three of the four
opposite-meaning pairs score **higher** than either genuine paraphrase.

That is not a quirk of these sentences. Negating a question changes one
short word, which moves a sentence embedding very little, while
rephrasing it changes most of the words, which moves the embedding a
lot. Cosine distance measures the wrong thing for this job.

The consequence is concrete. At a threshold of 0.90, tuned to catch the
paraphrases above, this gateway served the answer to "how do I enable
logging" when asked "how do I disable logging". It was observed, not
theorized.

## What Penstock does about it

- The semantic tier is **opt in**. Enabling caching gets the exact tier
  only.
- The configured threshold has a **floor of 0.95**, above the highest
  opposite pair measured rather than below the lowest paraphrase. The
  tier therefore fires rarely. That is the intended trade: when the
  failure mode is answering the opposite of the question, rarely is the
  right frequency.
- Turning it on logs a **warning at startup** naming the risk.
- Cache outcomes are separate metric events, so `semantic_hit` can be
  watched independently of `exact_hit` and a rising count is visible
  rather than buried in an aggregate hit rate.

## If you want it anyway

Some workloads genuinely tolerate it: a support bot answering FAQ
variants where every plausible neighbour has the same answer, or a
classification prompt with a small fixed output space. For those, the
tier is there.

What would make it safe more generally is a verification step, sending
the candidate hit and the new question to a small model to confirm they
are the same question. That costs an API call, which is the thing the
cache exists to avoid, so it is only worth it when the cached answer is
expensive. That work is not built.

## The honest summary

A semantic cache is easy to demo and hard to be right about. Published
hit rates are meaningless without the false-hit rate beside them, and
the false-hit rate is the number nobody publishes. This one is
measured, it is bad, and the feature ships configured accordingly.
