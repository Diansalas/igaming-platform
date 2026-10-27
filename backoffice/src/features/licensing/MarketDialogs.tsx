import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { isReasonCode, listJurisdictions, type LicenceDetail, type OperatingCountryPolicy, type TenantJurisdictionConfig } from '../../api/licensing'
import {
  classifyJurisdictionCreate,
  createJurisdiction,
  fileMarketChange,
  type ConfigAction,
  type FileMarketChangeBody,
  type JurisdictionCreateResult,
  type PolicyScope,
} from '../../api/marketConfig'
import { classifyBasisFiling, type BasisFilingResult } from '../../api/operatingBases'
import { listBrands } from '../../api/tenants'
import { Button } from '../../components/Button'
import { SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { UUID_RE } from './basisRules'
import { BasisFilingOutcome } from './BasisBits'
import { ApprovalRules, Disclaimer, LicensingErrorAlert, Success } from './LicensingBits'
import { REASON_CODE_HELP, safeToken } from './labels'
import {
  AUTH_REF_NOTE,
  CEILING_NOTE,
  CLASS_HINT,
  CONFIG_NOTE,
  COUNTRY_NOTE,
  JURISDICTION_NOTE,
  MARKET_DISCLAIMER,
  POLICY_NOTE,
  WIDENING_RULE,
} from './marketLabels'
import {
  ACTION_LABEL,
  authRefProblem,
  ceilingClass,
  ceilingNeedsRef,
  codeProblem,
  configClass,
  countryProblem,
  currencyListProblem,
  geoListProblem,
  optAuthRefProblem,
  optRegistryTextProblem,
  paymentListProblem,
  policyClass,
  policyNeedsRef,
  registryTextProblem,
  rulesetProblem,
  splitList,
  stateStatus,
  type MarketAction,
} from './marketRules'

function MarketDisclaimer() {
  return (
    <p role="note" aria-label="Market configuration disclaimer" className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
      {MARKET_DISCLAIMER}
    </p>
  )
}

/** One filing mutation: single-flight, result classified from the BODY, step-up handled by apiFetch (re-sent once). */
function useMarketFiling() {
  const qc = useQueryClient()
  const sending = useRef(false)
  const [result, setResult] = useState<BasisFilingResult | null>(null)
  const m = useMutation({
    mutationFn: (body: FileMarketChangeBody) => fileMarketChange(body),
    onSuccess: (body) => {
      setResult(classifyBasisFiling(body))
      void qc.invalidateQueries({ queryKey: ['licensing'] })
    },
    onSettled: () => {
      sending.current = false
    },
  })
  function submit(ready: boolean, body: () => FileMarketChangeBody) {
    if (!ready || sending.current || result) return
    sending.current = true
    m.mutate(body())
  }
  return { m, result, submit }
}

function Shell({
  title,
  onClose,
  onSubmit,
  ready,
  result,
  pending,
  error,
  submitLabel,
  children,
}: {
  title: string
  onClose: () => void
  onSubmit: () => void
  ready: boolean
  result: BasisFilingResult | null
  pending: boolean
  error: unknown
  submitLabel: string
  children: ReactNode
}) {
  return (
    <Modal title={title} onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          onSubmit()
        }}
      >
        <MarketDisclaimer />
        {children}
        <p className="text-xs text-slate-600">Filing needs a fresh second-factor check. The server decides whether this change widens or narrows and who must approve it.</p>
        {error && !result ? <LicensingErrorAlert error={error} /> : null}
        {result && <BasisFilingOutcome result={result} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>
            {result ? 'Close' : 'Cancel'}
          </Button>
          {!result && (
            <Button type="submit" disabled={!ready} isLoading={pending}>
              {submitLabel}
            </Button>
          )}
        </div>
      </form>
    </Modal>
  )
}

function ReasonField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const ok = isReasonCode(value)
  return (
    <>
      <TextInput label="Reason code" value={value} onChange={(e) => onChange(e.target.value.trim())} autoComplete="off" maxLength={64} help={REASON_CODE_HELP} aria-invalid={value !== '' && !ok} />
      {value !== '' && !ok && <p role="alert" className="text-sm text-red-700">That is not a valid reason code.</p>}
    </>
  )
}

function Problem({ text }: { text: string | null }) {
  return text ? <p role="alert" className="text-sm text-red-700">{text}</p> : null
}

function ClassHint({ cls }: { cls: 'widening' | 'narrowing' | 'neutral' | null }) {
  if (!cls) return null
  return (
    <p role="note" aria-label="Change class hint" className="text-sm text-slate-700">
      {CLASS_HINT[cls]} The server classifies it and decides.
    </p>
  )
}

function RuleNote({ label, children }: { label: string; children: ReactNode }) {
  return <p role="note" aria-label={label} className="text-xs text-slate-600">{children}</p>
}

const ACTIONS: MarketAction[] = ['enable', 'disable', 'withdraw']
const actionOptions = [{ value: '', label: 'Choose the action' }, ...ACTIONS.map((a) => ({ value: a, label: ACTION_LABEL[a] }))]

function AuthRefField({ value, onChange, required, problem }: { value: string; onChange: (v: string) => void; required: boolean; problem: string | null }) {
  return (
    <>
      <TextInput
        label={required ? 'Authorization reference (operator-supplied, required)' : 'Authorization reference (operator-supplied, optional)'}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="off"
        maxLength={500}
        help={required ? 'Required for this action. An opaque id of the decision record.' : 'Optional for this action. Blank means none.'}
      />
      {value !== '' && <Problem text={problem} />}
      <RuleNote label="Authorization reference note">{AUTH_REF_NOTE}</RuleNote>
    </>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------

/** File a licence country ceiling version (kind licence_ceiling_version). The licence is the page the operator opened it from. */
export function CeilingDialog({ licence, initialCountry, onClose }: { licence: LicenceDetail; initialCountry?: string; onClose: () => void }) {
  const { m, result, submit } = useMarketFiling()
  const [country, setCountry] = useState(initialCountry ?? '')
  const [action, setAction] = useState<MarketAction | ''>('')
  const [ref, setRef] = useState('')
  const [reason, setReason] = useState('')
  const needsRef = action !== '' && ceilingNeedsRef(action)
  const problems = {
    country: countryProblem(country),
    action: action === '' ? 'Choose the action.' : null,
    ref: needsRef ? authRefProblem(ref) : optAuthRefProblem(ref),
  }
  const ready = isReasonCode(reason) && Object.values(problems).every((p) => p === null)
  return (
    <Shell
      title="File a licence country ceiling change"
      onClose={onClose}
      onSubmit={() =>
        submit(ready, () => {
          const { state, status } = stateStatus(action as MarketAction)
          return {
            kind: 'licence_ceiling_version',
            reason_code: reason,
            payload: { licence_id: licence.id, country_code: country, state, status, authorization_reference: ref === '' ? null : ref },
          }
        })
      }
      ready={ready}
      result={result}
      pending={m.isPending}
      error={m.error}
      submitLabel="File ceiling change"
    >
      <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
        Licence: <span className="font-medium">{safeToken(licence.licence_number)}</span> <span className="font-mono text-xs">{licence.id}</span>
      </p>
      <RuleNote label="Ceiling note">{CEILING_NOTE}</RuleNote>
      <TextInput label="Country code" value={country} onChange={(e) => setCountry(e.target.value)} autoComplete="off" maxLength={2} help={COUNTRY_NOTE} />
      {country !== '' && <Problem text={problems.country} />}
      <SelectField label="Action" value={action} onChange={(e) => setAction(e.target.value as MarketAction | '')} options={actionOptions} />
      {action !== '' && <AuthRefField value={ref} onChange={setRef} required={needsRef} problem={problems.ref} />}
      <ReasonField value={reason} onChange={setReason} />
      <RuleNote label="Widening rule">{WIDENING_RULE}</RuleNote>
      {action !== '' && <ClassHint cls={ceilingClass(action)} />}
      <ApprovalRules />
    </Shell>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------

/** File an operating-country policy version (kind operating_policy_version) at the tenant, brand or operation rung. */
export function PolicyDialog({ tenantId, initial, onClose }: { tenantId: string; initial?: OperatingCountryPolicy; onClose: () => void }) {
  const { m, result, submit } = useMarketFiling()
  const [rung, setRung] = useState<PolicyScope | ''>(initial?.scope ?? '')
  const [brandScope, setBrandScope] = useState<'' | 'tenant' | 'brand'>(initial ? (initial.brand_id ? 'brand' : 'tenant') : '')
  const [brandId, setBrandId] = useState(initial?.brand_id ?? '')
  const [country, setCountry] = useState(initial?.country_code ?? '')
  const [operation, setOperation] = useState(initial?.operation_code ?? '')
  const [productScope, setProductScope] = useState<'' | 'all' | 'one'>(initial && initial.scope === 'operation' ? (initial.product_code ? 'one' : 'all') : '')
  const [product, setProduct] = useState(initial?.product_code ?? '')
  const [action, setAction] = useState<MarketAction | ''>('')
  const [ref, setRef] = useState('')
  const [reason, setReason] = useState('')

  const wantsBrand = rung === 'brand' || (rung === 'operation' && brandScope === 'brand')
  const brands = useQuery({ queryKey: ['licensing', 'policy-brands', tenantId], queryFn: () => listBrands(tenantId, { limit: 200 }), enabled: wantsBrand, retry: false })
  const needsRef = rung !== '' && action !== '' && policyNeedsRef(rung, action)
  const problems = {
    rung: rung === '' ? 'Choose the rung.' : null,
    brand: rung === 'operation' && brandScope === '' ? 'Choose whether this applies to the whole tenant or one brand.' : wantsBrand && !UUID_RE.test(brandId) ? 'Choose a brand.' : null,
    operation: rung === 'operation' ? codeProblem(operation) : null,
    product: rung === 'operation' ? (productScope === '' ? 'Choose all products or one product.' : productScope === 'one' ? codeProblem(product) : null) : null,
    country: countryProblem(country),
    action: action === '' ? 'Choose the action.' : null,
    ref: needsRef ? authRefProblem(ref) : optAuthRefProblem(ref),
  }
  const ready = isReasonCode(reason) && Object.values(problems).every((p) => p === null)

  return (
    <Shell
      title="File an operating-country policy change"
      onClose={onClose}
      onSubmit={() =>
        submit(ready, () => {
          const { state, status } = stateStatus(action as MarketAction)
          return {
            kind: 'operating_policy_version',
            reason_code: reason,
            payload: {
              tenant_id: tenantId,
              scope: rung as PolicyScope,
              brand_id: wantsBrand ? brandId : null,
              operation_code: rung === 'operation' ? operation : null,
              product_code: rung === 'operation' && productScope === 'one' ? product : null,
              country_code: country,
              state,
              status,
              authorization_reference: ref === '' ? null : ref,
            },
          }
        })
      }
      ready={ready}
      result={result}
      pending={m.isPending}
      error={m.error}
      submitLabel="File policy change"
    >
      <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
        Tenant: <span className="font-mono text-xs">{tenantId}</span>
      </p>
      <RuleNote label="Policy note">{POLICY_NOTE}</RuleNote>
      <SelectField
        label="Rung"
        value={rung}
        onChange={(e) => {
          setRung(e.target.value as PolicyScope | '')
          setBrandScope('')
          setBrandId('')
          setOperation('')
          setProductScope('')
          setProduct('')
        }}
        options={[
          { value: '', label: 'Choose the rung' },
          { value: 'tenant', label: 'Tenant (opts the country in for the whole tenant)' },
          { value: 'brand', label: 'Brand (narrows for one brand)' },
          { value: 'operation', label: 'Operation (narrows for one operation, optionally one brand and one product)' },
        ]}
        help="Chosen explicitly. There is no default rung."
      />
      {rung === 'operation' && (
        <SelectField
          label="Brand scope"
          value={brandScope}
          onChange={(e) => {
            setBrandScope(e.target.value as '' | 'tenant' | 'brand')
            setBrandId('')
          }}
          options={[{ value: '', label: 'Choose the brand scope' }, { value: 'tenant', label: 'The whole tenant (all brands)' }, { value: 'brand', label: 'One brand only' }]}
        />
      )}
      {wantsBrand && (
        <SelectField
          label="Brand"
          value={brandId}
          onChange={(e) => setBrandId(e.target.value)}
          options={[{ value: '', label: 'Choose a brand' }, ...(brands.data?.items ?? []).map((b) => ({ value: b.id, label: `${b.name} (${b.slug})` }))]}
        />
      )}
      {brands.error ? <p className="text-sm text-red-700">The brand list could not be loaded.</p> : null}
      {rung === 'operation' && (
        <>
          <TextInput label="Operation code" value={operation} onChange={(e) => setOperation(e.target.value)} autoComplete="off" maxLength={64} help="An operation from the platform vocabulary, for example registration, deposit or casino_wagering. The server refuses an unknown one." />
          {operation !== '' && <Problem text={problems.operation} />}
          <SelectField
            label="Product scope"
            value={productScope}
            onChange={(e) => setProductScope(e.target.value as '' | 'all' | 'one')}
            options={[{ value: '', label: 'Choose the product scope' }, { value: 'all', label: 'All products' }, { value: 'one', label: 'One product only' }]}
          />
          {productScope === 'one' && <TextInput label="Product code" value={product} onChange={(e) => setProduct(e.target.value)} autoComplete="off" maxLength={64} />}
          {productScope === 'one' && product !== '' && <Problem text={problems.product} />}
        </>
      )}
      <TextInput label="Country code" value={country} onChange={(e) => setCountry(e.target.value)} autoComplete="off" maxLength={2} help={COUNTRY_NOTE} />
      {country !== '' && <Problem text={problems.country} />}
      <SelectField label="Action" value={action} onChange={(e) => setAction(e.target.value as MarketAction | '')} options={actionOptions} />
      {action === 'withdraw' && rung !== '' && (
        <RuleNote label="Withdrawal note">
          {rung === 'tenant'
            ? 'Withdrawing a tenant-rung version leaves the country with no tenant opt-in, which is a terminal block (narrowing).'
            : 'Withdrawing a brand or operation version removes a block, so the country is then inherited from the rung above (widening, and an authorization reference is required).'}
        </RuleNote>
      )}
      {action !== '' && rung !== '' && <AuthRefField value={ref} onChange={setRef} required={needsRef} problem={problems.ref} />}
      <ReasonField value={reason} onChange={setReason} />
      <RuleNote label="Widening rule">{WIDENING_RULE}</RuleNote>
      {action !== '' && rung !== '' && <ClassHint cls={policyClass(rung, action)} />}
      <ApprovalRules />
    </Shell>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------

const join = (a: string[]) => a.join(', ')

/** File a tenant jurisdiction configuration version (kind tenant_jurisdiction_config_version): set a new version, or close. */
export function ConfigDialog({
  tenantId,
  configs,
  initialJurisdictionId,
  initialAction,
  onClose,
}: {
  tenantId: string
  /** Versions already shown on the page; the one in force (effective_to null) for the chosen jurisdiction loads into the form. */
  configs: TenantJurisdictionConfig[]
  initialJurisdictionId?: string
  initialAction?: ConfigAction
  onClose: () => void
}) {
  const { m, result, submit } = useMarketFiling()
  const open = (jid: string) => configs.find((c) => c.jurisdiction_id === jid && c.effective_to === null) ?? null
  const start = initialJurisdictionId ? open(initialJurisdictionId) : null
  const [action, setAction] = useState<ConfigAction | ''>(initialAction ?? '')
  const [jurisdictionId, setJurisdictionId] = useState(initialJurisdictionId ?? '')
  const [kyc, setKyc] = useState(start?.kyc_ruleset_id ?? '')
  const [aml, setAml] = useState(start?.aml_ruleset_id ?? '')
  const [rg, setRg] = useState(start?.rg_ruleset_id ?? '')
  const [reporting, setReporting] = useState(start?.reporting_ruleset_id ?? '')
  const [currencies, setCurrencies] = useState(start ? join(start.allowed_currencies) : '')
  const [payments, setPayments] = useState(start ? join(start.allowed_payment_methods) : '')
  const [geo, setGeo] = useState(start ? join(start.geo_block_list) : '')
  const [reason, setReason] = useState('')

  const jurisdictions = useQuery({ queryKey: ['licensing', 'jurisdictions'], queryFn: listJurisdictions, retry: false })
  const current = jurisdictionId === '' ? null : open(jurisdictionId)

  // Choosing a jurisdiction loads the version in force, so a blank field is always a deliberate removal and never an accident.
  function pickJurisdiction(id: string) {
    setJurisdictionId(id)
    const c = id === '' ? null : open(id)
    setKyc(c?.kyc_ruleset_id ?? '')
    setAml(c?.aml_ruleset_id ?? '')
    setRg(c?.rg_ruleset_id ?? '')
    setReporting(c?.reporting_ruleset_id ?? '')
    setCurrencies(c ? join(c.allowed_currencies) : '')
    setPayments(c ? join(c.allowed_payment_methods) : '')
    setGeo(c ? join(c.geo_block_list) : '')
  }

  const isSet = action === 'set'
  const problems = {
    action: action === '' ? 'Choose the action.' : null,
    jurisdiction: UUID_RE.test(jurisdictionId) ? null : 'Choose a jurisdiction.',
    kyc: isSet ? rulesetProblem(kyc) : null,
    aml: isSet ? rulesetProblem(aml) : null,
    rg: isSet ? rulesetProblem(rg) : null,
    reporting: isSet ? rulesetProblem(reporting) : null,
    currencies: isSet ? currencyListProblem(currencies) : null,
    payments: isSet ? paymentListProblem(payments) : null,
    geo: isSet ? geoListProblem(geo) : null,
  }
  const ready = isReasonCode(reason) && Object.values(problems).every((p) => p === null)
  const nil = (s: string) => (s === '' ? null : s)
  const cls =
    action === 'close'
      ? ('narrowing' as const)
      : isSet && Object.values(problems).every((p) => p === null)
        ? configClass(current, { kyc: nil(kyc), aml: nil(aml), rg: nil(rg), reporting: nil(reporting), currencies: splitList(currencies), payments: splitList(payments), geo: splitList(geo) })
        : null

  return (
    <Shell
      title="File a tenant jurisdiction configuration change"
      onClose={onClose}
      onSubmit={() =>
        submit(ready, () => ({
          kind: 'tenant_jurisdiction_config_version',
          reason_code: reason,
          payload: {
            tenant_id: tenantId,
            jurisdiction_id: jurisdictionId,
            action: action as ConfigAction,
            kyc_ruleset_id: isSet ? nil(kyc) : null,
            aml_ruleset_id: isSet ? nil(aml) : null,
            rg_ruleset_id: isSet ? nil(rg) : null,
            reporting_ruleset_id: isSet ? nil(reporting) : null,
            allowed_currencies: isSet ? splitList(currencies) : null,
            allowed_payment_methods: isSet ? splitList(payments) : null,
            geo_block_list: isSet ? splitList(geo) : null,
          },
        }))
      }
      ready={ready}
      result={result}
      pending={m.isPending}
      error={m.error}
      submitLabel="File configuration change"
    >
      <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
        Tenant: <span className="font-mono text-xs">{tenantId}</span>
      </p>
      <RuleNote label="Configuration note">{CONFIG_NOTE}</RuleNote>
      <SelectField
        label="Action"
        value={action}
        onChange={(e) => setAction(e.target.value as ConfigAction | '')}
        options={[{ value: '', label: 'Choose the action' }, { value: 'set', label: 'Set a new version (replaces the version in force)' }, { value: 'close', label: 'Close the version in force (the tenant then has no configuration here)' }]}
        disabled={!!initialAction}
      />
      <SelectField
        label="Jurisdiction"
        value={jurisdictionId}
        onChange={(e) => pickJurisdiction(e.target.value)}
        options={[{ value: '', label: 'Choose a jurisdiction' }, ...(jurisdictions.data ?? []).map((j) => ({ value: j.id, label: `${safeToken(j.code)} - ${j.name}` }))]}
      />
      {jurisdictions.error ? <p className="text-sm text-red-700">The jurisdiction list could not be loaded.</p> : null}
      {jurisdictionId !== '' && action !== '' && (
        <p role="note" aria-label="Version in force" className="text-sm text-slate-700">
          {current ? 'A version is in force for this jurisdiction and is loaded below.' : 'No version is in force for this jurisdiction on the page shown (it may be on another page of the list).'}
        </p>
      )}
      {action === 'close' && <p role="note" aria-label="Close note" className="text-sm text-slate-700">Closing carries no content. It is a narrowing change applied at once by one platform administrator.</p>}
      {isSet && (
        <>
          <TextInput label="KYC ruleset id (optional)" value={kyc} onChange={(e) => setKyc(e.target.value)} autoComplete="off" maxLength={128} help="An opaque id. Blank means none." />
          <Problem text={kyc !== '' ? problems.kyc : null} />
          <TextInput label="AML ruleset id (optional)" value={aml} onChange={(e) => setAml(e.target.value)} autoComplete="off" maxLength={128} help="An opaque id. Blank means none." />
          <Problem text={aml !== '' ? problems.aml : null} />
          <TextInput label="Responsible gaming ruleset id (optional)" value={rg} onChange={(e) => setRg(e.target.value)} autoComplete="off" maxLength={128} help="An opaque id. Blank means none." />
          <Problem text={rg !== '' ? problems.rg : null} />
          <TextInput label="Reporting ruleset id (optional)" value={reporting} onChange={(e) => setReporting(e.target.value)} autoComplete="off" maxLength={128} help="An opaque id. Blank means none." />
          <Problem text={reporting !== '' ? problems.reporting : null} />
          <TextInput label="Allowed currencies" value={currencies} onChange={(e) => setCurrencies(e.target.value)} autoComplete="off" help="Comma separated asset codes, for example the codes of active assets. Blank means none. The server requires each to be an active asset." />
          <Problem text={currencies !== '' ? problems.currencies : null} />
          <TextInput label="Allowed payment methods" value={payments} onChange={(e) => setPayments(e.target.value)} autoComplete="off" help="Comma separated codes (lower-case letters, digits, _). Blank means none." />
          <Problem text={payments !== '' ? problems.payments : null} />
          <TextInput label="Geo block list" value={geo} onChange={(e) => setGeo(e.target.value)} autoComplete="off" help="Comma separated ISO 3166-1 alpha-2 country codes whose players are blocked. Blank means none. Removing a code widens." />
          <Problem text={geo !== '' ? problems.geo : null} />
        </>
      )}
      <ReasonField value={reason} onChange={setReason} />
      <RuleNote label="Widening rule">{WIDENING_RULE}</RuleNote>
      <ClassHint cls={cls} />
      <ApprovalRules />
    </Shell>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------

/** Create a jurisdiction registry row (POST /v1/admin/jurisdictions). Direct, single-actor, step-up critical; NOT four-eyes gated. */
export function JurisdictionDialog({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const sending = useRef(false)
  const [code, setCode] = useState('')
  const [name, setName] = useState('')
  const [body, setBody] = useState('')
  const [notes, setNotes] = useState('')
  const [reason, setReason] = useState('')
  const [result, setResult] = useState<JurisdictionCreateResult | null>(null)
  const problems = {
    code: registryTextProblem(code, 64),
    name: registryTextProblem(name, 200),
    body: optRegistryTextProblem(body, 200),
    notes: optRegistryTextProblem(notes, 1000),
  }
  const ready = isReasonCode(reason) && Object.values(problems).every((p) => p === null)
  const m = useMutation({
    mutationFn: () => {
      const b: Parameters<typeof createJurisdiction>[0] = { code, name, reason_code: reason }
      if (body !== '') b.regulatory_body = body
      if (notes !== '') b.notes = notes
      return createJurisdiction(b)
    },
    onSuccess: (r) => {
      setResult(classifyJurisdictionCreate(r))
      void qc.invalidateQueries({ queryKey: ['licensing'] })
    },
    onSettled: () => {
      sending.current = false
    },
  })
  return (
    <Modal title="Create a jurisdiction" onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!ready || sending.current || result) return
          sending.current = true
          m.mutate()
        }}
      >
        <Disclaimer />
        <RuleNote label="Jurisdiction note">{JURISDICTION_NOTE}</RuleNote>
        <TextInput label="Code" value={code} onChange={(e) => setCode(e.target.value)} autoComplete="off" maxLength={64} help="A short unique code of your own scheme, typed by you (for example the code the regulator or your register uses). Nothing is suggested." />
        {code !== '' && <Problem text={problems.code} />}
        <TextInput label="Name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" maxLength={200} />
        {name !== '' && <Problem text={problems.name} />}
        <TextInput label="Regulatory body (optional)" value={body} onChange={(e) => setBody(e.target.value)} autoComplete="off" maxLength={200} help="As recorded by you. Blank means not recorded." />
        {body !== '' && <Problem text={problems.body} />}
        <TextInput label="Notes (optional)" value={notes} onChange={(e) => setNotes(e.target.value)} autoComplete="off" maxLength={1000} help="Bounded note, no personal data. Blank means none." />
        {notes !== '' && <Problem text={problems.notes} />}
        <ReasonField value={reason} onChange={setReason} />
        <p className="text-xs text-slate-600">Filing needs a fresh second-factor check. A duplicate code is refused.</p>
        {m.error && !result ? <LicensingErrorAlert error={m.error} /> : null}
        {result?.kind === 'created' && (
          <Success>
            Jurisdiction recorded.{' '}
            <Link className="font-medium underline" to={`/licensing/jurisdictions/${encodeURIComponent(result.jurisdiction.id)}`}>Open {safeToken(result.jurisdiction.code)}</Link>
          </Success>
        )}
        {result?.kind === 'unconfirmed' && <p role="status" className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900">The server answered, but the answer could not be recognised. Nothing is confirmed. Check the jurisdiction list before trying again.</p>}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>
            {result ? 'Close' : 'Cancel'}
          </Button>
          {!result && (
            <Button type="submit" disabled={!ready} isLoading={m.isPending}>
              Create jurisdiction
            </Button>
          )}
        </div>
      </form>
    </Modal>
  )
}
