import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import {
  cancelMyRgLimitChange,
  confirmMyRgLimitChange,
  getMyRgLimits,
  setMyRgLimit,
  type RgKind,
  type RgLimitChange,
  type RgLimitOption,
  type RgLimitState,
  type RgOptions,
  type RgPeriod,
} from "../../api/rg";
import { Badge } from "../../components/Badge";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import { ErrorState } from "../../components/ErrorState";
import { LoadingSpinner } from "../../components/LoadingSpinner";
import { Select } from "../../components/Select";
import { TextInput } from "../../components/TextInput";
import {
  formatMinorString,
  majorToMinorString,
  minorGreater,
} from "../../lib/money";
import { newIdempotencyKey } from "../../lib/idempotencyKey";
import {
  KIND_LABELS,
  PERIOD_LABELS,
  formatSeconds,
  formatWhen,
  rgErrorMessage,
  useAttemptKey,
} from "./rgSupport";

function stateFor(
  limits: RgLimitState[],
  asset: string,
  kind: string,
  period: string,
): RgLimitState | undefined {
  return limits.find(
    (l) => l.asset_code === asset && l.kind === kind && l.period === period,
  );
}

/**
 * View and change the deposit, loss and wager limits. Every number shown is the server's: the REALIZED value
 * (the one enforced), the open pending change and when it takes effect. The form only sends the player's
 * request; whether it is a decrease (effective immediately) or an increase (after a cooling-off period, set
 * by the server, and possibly needing the player's own confirmation) is decided and reported by the server.
 * The hint under the form is a preview of that rule, not the rule.
 */
export function LimitsCard({
  assetCode,
  options,
}: {
  assetCode: string;
  options: RgOptions;
}) {
  const queryClient = useQueryClient();
  const limitsQuery = useQuery({
    queryKey: ["rg-limits"],
    queryFn: getMyRgLimits,
  });
  const attempt = useAttemptKey();

  // A kind whose rulebook cannot be completed (unavailable_reason) is not offered; time-out and self-exclusion are separate cards.
  const offered = options.limits.filter((o) => !o.unavailable_reason);
  const unavailable = options.limits.filter((o) => !!o.unavailable_reason);
  const [kind, setKind] = useState<RgKind>(offered[0]?.kind ?? "deposit");
  const option: RgLimitOption | undefined = offered.find(
    (o) => o.kind === kind,
  );
  const [period, setPeriod] = useState<RgPeriod | "">("");
  const [amount, setAmount] = useState("");
  const [result, setResult] = useState<RgLimitChange | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);

  const effectivePeriod: RgPeriod | "" =
    period && option?.allowed_periods.includes(period) ? period : "";
  const minor =
    amount.trim() === "" ? null : majorToMinorString(amount, assetCode);
  const existing = effectivePeriod
    ? stateFor(limitsQuery.data?.limits ?? [], assetCode, kind, effectivePeriod)
    : undefined;
  const isIncrease = !!(
    minor &&
    existing?.value &&
    minorGreater(minor, existing.value)
  );

  const setMutation = useMutation({
    mutationFn: (body: {
      kind: RgKind;
      period: RgPeriod;
      value: string | null;
    }) =>
      setMyRgLimit(
        {
          kind: body.kind,
          asset_code: assetCode,
          period: body.period,
          value: body.value,
        },
        attempt.keyFor(
          `${body.kind}|${assetCode}|${body.period}|${body.value}`,
        ),
      ),
    onSuccess: async (change) => {
      attempt.reset();
      setResult(change);
      setAmount("");
      await queryClient.invalidateQueries({ queryKey: ["rg-limits"] });
    },
  });

  const resolveMutation = useMutation({
    mutationFn: ({
      id,
      intent,
    }: {
      id: string;
      intent: "confirm" | "cancel";
    }) =>
      intent === "confirm"
        ? confirmMyRgLimitChange(id, newIdempotencyKey())
        : cancelMyRgLimitChange(id, newIdempotencyKey()),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["rg-limits"] });
    },
  });

  function submit(value: string | null, forPeriod: RgPeriod) {
    setLocalError(null);
    setResult(null);
    setMutationReset();
    setMutation.mutate({ kind, period: forPeriod, value });
  }
  function setMutationReset() {
    setMutation.reset();
    resolveMutation.reset();
  }

  function onSet() {
    if (!effectivePeriod) {
      setLocalError("Choose a period.");
      return;
    }
    if (minor === null) {
      setLocalError("Enter a positive amount (up to the currency’s decimals).");
      return;
    }
    submit(minor, effectivePeriod);
  }

  const rows = (limitsQuery.data?.limits ?? []).filter(
    (l) => l.asset_code === assetCode && (l.has_value || l.pending),
  );
  const now = Date.now();

  return (
    <Card title={`Deposit, loss and wager limits (${assetCode})`}>
      <div className="flex flex-col gap-4 text-sm">
        {!options.limit_enforcement_enabled && (
          <p className="rounded-md border border-slate-200 bg-slate-50 p-3 text-slate-700">
            Limits you set are recorded on your account. Automatic enforcement
            of deposit, loss and wager limits has not been switched on for this
            brand yet.
          </p>
        )}
        {unavailable.length > 0 && (
          <p
            role="note"
            className="rounded-md border border-amber-200 bg-amber-50 p-3 text-amber-900"
          >
            Setting or changing{" "}
            {unavailable
              .map((o) => (KIND_LABELS[o.kind] ?? o.kind).toLowerCase())
              .join(", ")}{" "}
            limits is not available right now. Your existing limits stay in
            force, you can still lower them, and the time-out and self-exclusion
            tools below are unaffected.
          </p>
        )}
        {offered.length > 0 && offered.every((o) => !o.config_in_force) && (
          <p className="text-slate-600">
            No limit rules are configured for {assetCode} yet. A limit you set
            is still recorded, and decreases take effect at once.
          </p>
        )}

        {limitsQuery.isLoading && <LoadingSpinner label="Loading limits..." />}
        {limitsQuery.error && (
          <ErrorState
            error={limitsQuery.error}
            onRetry={() => void limitsQuery.refetch()}
          />
        )}
        {limitsQuery.data && rows.length === 0 && (
          <p className="text-slate-600">
            You have not set any limits for {assetCode}.
          </p>
        )}
        {rows.length > 0 && (
          <ul
            className="flex flex-col divide-y divide-border"
            aria-label="Current limits"
          >
            {rows.map((l) => {
              const p = l.pending;
              const confirmable =
                !!p &&
                p.requires_confirmation &&
                new Date(p.effective_at).getTime() <= now;
              return (
                <li
                  key={`${l.kind}|${l.scope}|${l.period}`}
                  className="flex flex-col gap-2 py-3"
                >
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium text-slate-900">
                      {KIND_LABELS[l.kind] ?? l.kind}
                    </span>
                    <span className="text-slate-500">
                      {PERIOD_LABELS[l.period ?? ""] ?? l.period}
                    </span>
                    <span className="text-slate-900">
                      {l.has_value && l.value
                        ? formatMinorString(l.value, assetCode)
                        : "No limit in force"}
                    </span>
                    {l.scope === "tenant_person" && (
                      <Badge tone="info">
                        Across all your accounts with this operator
                      </Badge>
                    )}
                  </div>
                  {p && (
                    <div className="flex flex-wrap items-center gap-2 rounded-md bg-amber-50 p-2 text-amber-900">
                      <span>
                        {p.value === null
                          ? "Removal"
                          : `Increase to ${formatMinorString(p.value, assetCode)}`}{" "}
                        pending: takes effect {formatWhen(p.effective_at)}
                        {p.requires_confirmation
                          ? " and then needs your confirmation"
                          : ""}
                        .
                      </span>
                      {confirmable && (
                        <Button
                          variant="secondary"
                          isLoading={resolveMutation.isPending}
                          onClick={() =>
                            resolveMutation.mutate({
                              id: p.id,
                              intent: "confirm",
                            })
                          }
                        >
                          Confirm change
                        </Button>
                      )}
                      <Button
                        variant="ghost"
                        disabled={resolveMutation.isPending}
                        onClick={() =>
                          resolveMutation.mutate({ id: p.id, intent: "cancel" })
                        }
                      >
                        Cancel pending change
                      </Button>
                    </div>
                  )}
                  {l.has_value && !p && l.period && (
                    <div>
                      <Button
                        variant="ghost"
                        disabled={setMutation.isPending}
                        onClick={() => {
                          setKind(l.kind as RgKind);
                          submit(null, l.period as RgPeriod);
                        }}
                      >
                        Remove limit
                      </Button>
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        )}
        {resolveMutation.error && (
          <p role="alert" className="text-red-700">
            {rgErrorMessage(resolveMutation.error)}
          </p>
        )}

        {offered.length > 0 && (
          <div
            className="flex flex-col gap-3 border-t border-border pt-4"
            role="group"
            aria-label="Set a limit"
          >
            <h4 className="font-medium text-slate-800">
              Set or change a limit
            </h4>
            <div className="flex flex-wrap items-end gap-3">
              <label className="flex flex-col gap-1">
                <span className="text-slate-600">Limit</span>
                <Select
                  aria-label="Limit kind"
                  value={kind}
                  onChange={(e) => {
                    setKind(e.target.value as RgKind);
                    setPeriod("");
                  }}
                  options={offered.map((o) => ({
                    value: o.kind,
                    label: KIND_LABELS[o.kind] ?? o.kind,
                  }))}
                />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-slate-600">Period</span>
                <Select
                  aria-label="Limit period"
                  value={effectivePeriod}
                  onChange={(e) => setPeriod(e.target.value as RgPeriod)}
                  placeholder="Choose..."
                  options={(option?.allowed_periods ?? []).map((p) => ({
                    value: p,
                    label: PERIOD_LABELS[p] ?? p,
                  }))}
                />
              </label>
              <label className="flex flex-col gap-1">
                <span className="text-slate-600">Amount ({assetCode})</span>
                <TextInput
                  aria-label="Limit amount"
                  inputMode="decimal"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                />
              </label>
              <Button isLoading={setMutation.isPending} onClick={onSet}>
                Set limit
              </Button>
            </div>
            {option && (
              <ul className="list-disc pl-5 text-xs text-slate-500">
                {option.mandatory && (
                  <li>
                    A {KIND_LABELS[option.kind]?.toLowerCase()} is required
                    before you can{" "}
                    {option.kind === "deposit" ? "deposit" : "play"}.
                  </li>
                )}
                {option.ceiling && (
                  <li>
                    Maximum allowed:{" "}
                    {formatMinorString(option.ceiling, assetCode)}.
                  </li>
                )}
                <li>Lowering a limit takes effect immediately.</li>
                <li>
                  Raising or removing a limit only takes effect after a
                  cooling-off period
                  {option.increase_cooldown_seconds
                    ? ` of ${formatSeconds(option.increase_cooldown_seconds)}`
                    : ""}
                  , and you may be asked to confirm it.
                </li>
              </ul>
            )}
            {isIncrease && (
              <p role="note" className="text-amber-800">
                This is higher than your current limit, so it will not take
                effect immediately.
              </p>
            )}
            {localError && (
              <p role="alert" className="text-red-700">
                {localError}
              </p>
            )}
            {setMutation.error && (
              <p role="alert" className="text-red-700">
                {rgErrorMessage(setMutation.error)}
              </p>
            )}
            {result && (
              <p role="status" className="text-emerald-700">
                {result.pending
                  ? `Your request was received. It takes effect ${formatWhen(result.effective_at)}${result.requires_confirmation ? " and then needs your confirmation" : ""}. Your current limit stays in force until then.`
                  : result.value === null
                    ? "The limit was removed."
                    : `Your limit is now ${formatMinorString(result.value, assetCode)}, effective immediately.`}
              </p>
            )}
          </div>
        )}
      </div>
    </Card>
  );
}
