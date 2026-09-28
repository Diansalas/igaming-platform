-- PRH-2 K2 (ADR 0100): governed manual adjustments. CHECKPOINT CONTENT:
-- this commit carries only the K2-G1 hard-gate fences (security K1
-- re-check); the rest of 0113 lands in the next commit of this unmerged
-- branch.
--
-- K2-G1: AS RESTRICTIVE acting SELECT fences on asset_operation_eligibility
-- and open_bet_self_exclusion_policies, whose SELECT NULL-tenant arms the
-- hardened A-18 found reachable by an acting session (ADR 0099 §6.2 had
-- judged them by their WRITE policies only).

CREATE POLICY acting_fence_select ON asset_operation_eligibility AS RESTRICTIVE FOR SELECT
    USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_select ON open_bet_self_exclusion_policies AS RESTRICTIVE FOR SELECT
    USING (NOT financial_acting_gucs_present());
