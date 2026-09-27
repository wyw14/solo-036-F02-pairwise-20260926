PigFlow is a local HTTP control service for long-distance pipeline pigging operations.

## Tracking lineage corrections

Tracking points record their physical probe and generation. When a probe
number is entered incorrectly the original observation is never deleted:

- `POST /api/operations/{runID}/tracking/withdraw` withdraws an observation
  (and anything derived from it), optionally attaching a corrected
  observation with the right probe. The original stays in the lineage with
  `withdrawn`, `withdrawn_reason` and `corrected_by` links, while the
  correction records `corrects_observation_id`.
- Withdrawn points are removed from the live evidence set and the passage
  basis (quorum, physical-source independence, continuity) is recomputed
  from the remaining observations.
- Run decisions that relied on the withdrawn evidence — repressurization
  permits and the affected tracking rows — are flagged `review_required`
  and an open `RunReview` is attached to the operation.
- `POST /api/operations/{runID}/passage/reassess` recomputes the basis and
  reports each dimension; the review stays open even after the basis holds
  again.
- `POST /api/operations/{runID}/reviews/resolve` closes the review after
  operator confirmation and clears the review flags.
- `GET /api/operations/{runID}/observations?station_id=...` returns the full
  observation history, withdrawn originals and corrections included.
