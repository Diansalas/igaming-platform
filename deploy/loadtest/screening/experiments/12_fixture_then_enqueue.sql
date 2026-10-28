-- Runs the randomised fixture of 04 (4 tenants x 3 jurisdictions, never-in-force versions, ties, future-effective versions, backfill-grace tenant, persons
-- without an account, sanctions-class resolution by a match) and then the enqueue-candidate comparison of 06 (production text vs guarded) on it, so E2 is
-- also tested on multi-version random histories (not only on 3 uniform versions).  ./run.sh explain-src capa_base experiments/12_fixture_then_enqueue.sql
\i 04_gate_equiv.sql
\i 06_enqueue_candidates.sql
