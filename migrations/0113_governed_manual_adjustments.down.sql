-- Reverses the checkpoint content of 0113 (K2-G1 fences only).
DROP POLICY acting_fence_select ON asset_operation_eligibility;
DROP POLICY acting_fence_select ON open_bet_self_exclusion_policies;
