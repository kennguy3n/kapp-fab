import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Lock, ShieldCheck, X } from "lucide-react";
import type { PlacementPolicy } from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Field,
  Input,
  Select,
  Skeleton,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { humanizeToken } from "../lib/ktypeView";
import { tenantKey, useTenantName } from "../lib/tenant";
import { AdminErrorState, AdminPageHeader } from "./adminKit";

const ENCRYPTION_MODES: { value: string; label: string; help: string }[] = [
  {
    value: "managed",
    label: "Managed — gateway-side keys",
    help: "Kapp manages encryption keys. Server-side search and indexing stay available.",
  },
  {
    value: "client_side",
    label: "Client-side — zero access",
    help: "Keys never leave the customer. Highest privacy; some server features are unavailable.",
  },
  {
    value: "public_distribution",
    label: "Public distribution",
    help: "Content is intended for public distribution and is not encrypted at rest.",
  },
];

function modeLabel(mode: string): string {
  if (!mode) return "—";
  // Known modes get their friendly label; anything unexpected from the
  // fabric still reads as Title Case rather than a raw enum token.
  return ENCRYPTION_MODES.find((m) => m.value === mode)?.label ?? humanizeToken(mode);
}

interface PolicyForm {
  mode: string;
  kms: string;
  providers: string[];
  regions: string[];
  countries: string[];
  storageClasses: string[];
  cacheLocation: string;
}

function toForm(p: PlacementPolicy): PolicyForm {
  return {
    mode: p.policy.encryption.mode ?? "",
    kms: p.policy.encryption.kms ?? "",
    providers: p.policy.placement.provider ?? [],
    regions: p.policy.placement.region ?? [],
    countries: p.policy.placement.country ?? [],
    storageClasses: p.policy.placement.storage_class ?? [],
    cacheLocation: p.policy.placement.cache_location ?? "",
  };
}

function toPolicy(base: PlacementPolicy, form: PolicyForm): PlacementPolicy {
  return {
    tenant: base.tenant,
    ...(base.bucket ? { bucket: base.bucket } : {}),
    policy: {
      encryption: {
        mode: form.mode,
        ...(form.kms.trim() ? { kms: form.kms.trim() } : {}),
      },
      placement: {
        provider: form.providers,
        ...(form.regions.length ? { region: form.regions } : {}),
        ...(form.countries.length ? { country: form.countries } : {}),
        ...(form.storageClasses.length
          ? { storage_class: form.storageClasses }
          : {}),
        ...(form.cacheLocation.trim()
          ? { cache_location: form.cacheLocation.trim() }
          : {}),
      },
    },
  };
}

export function PlacementPolicyPage() {
  const qc = useQueryClient();
  const tenantId = tenantKey();
  const { name: tenantName } = useTenantName();

  const policyQuery = useQuery({
    queryKey: ["placement-policy", tenantId],
    queryFn: () => api.getPlacementPolicy(tenantId),
  });

  const [form, setForm] = useState<PolicyForm | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const [readOnly, setReadOnly] = useState(false);

  useEffect(() => {
    if (policyQuery.data) {
      setForm(toForm(policyQuery.data));
      setSubmitted(false);
    }
  }, [policyQuery.data]);

  const update = useMutation({
    mutationFn: (policy: PlacementPolicy) =>
      api.updatePlacementPolicy(tenantId, policy),
    onSuccess: () => {
      toast.success("Placement policy saved");
      void qc.invalidateQueries({ queryKey: ["placement-policy", tenantId] });
    },
    onError: (err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err);
      if (/paid plan|free/i.test(msg)) {
        setReadOnly(true);
        toast.error("Editing the placement policy requires a paid plan");
      } else {
        toast.error(msg);
      }
    },
  });

  const dirty = useMemo(() => {
    if (!policyQuery.data || !form) return false;
    return (
      JSON.stringify(toPolicy(policyQuery.data, form)) !==
      JSON.stringify(policyQuery.data)
    );
  }, [form, policyQuery.data]);

  const countryError =
    submitted && form?.countries.some((c) => !/^[A-Z]{2}$/.test(c))
      ? "Country codes must be two letters (ISO-3166), e.g. US, GB, SG."
      : undefined;
  const modeError = submitted && !form?.mode ? "Choose an encryption mode" : undefined;
  const providerError =
    submitted && form && form.providers.length === 0
      ? "Add at least one storage provider"
      : undefined;

  function save() {
    if (!policyQuery.data || !form) return;
    setSubmitted(true);
    if (
      !form.mode ||
      form.providers.length === 0 ||
      form.countries.some((c) => !/^[A-Z]{2}$/.test(c))
    ) {
      return;
    }
    update.mutate(toPolicy(policyQuery.data, form));
  }

  const isFreePlanError =
    policyQuery.error instanceof Error &&
    /paid plan|free/i.test(policyQuery.error.message);

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Data residency"
        description="Control where this workspace's data lives and how it's encrypted in the ZK Object Fabric. Changes are forwarded to the fabric and applied to new objects."
        actions={
          <Badge variant="neutral" size="md">
            {tenantName}
          </Badge>
        }
      />

      {policyQuery.isLoading ? (
        <Card>
          <CardContent className="flex flex-col gap-4 pt-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} variant="rect" className="h-10 w-full" />
            ))}
          </CardContent>
        </Card>
      ) : isFreePlanError ? (
        <UpgradeNotice />
      ) : policyQuery.error ? (
        <AdminErrorState
          title="Couldn't load the placement policy"
          error={policyQuery.error}
          onRetry={() => policyQuery.refetch()}
        />
      ) : form ? (
        <>
          {readOnly && <UpgradeNotice />}
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Lock className="h-4 w-4 text-fg-muted" aria-hidden="true" />
                Encryption
              </CardTitle>
            </CardHeader>
            <CardContent className="flex flex-col gap-4">
              <Field
                label="Encryption mode"
                required
                error={modeError}
                help={
                  ENCRYPTION_MODES.find((m) => m.value === form.mode)?.help
                }
              >
                <Select
                  value={form.mode}
                  disabled={readOnly}
                  onChange={(e) =>
                    setForm({ ...form, mode: e.target.value })
                  }
                >
                  <option value="" disabled>
                    Select a mode…
                  </option>
                  {ENCRYPTION_MODES.map((m) => (
                    <option key={m.value} value={m.value}>
                      {m.label}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label="Key management reference"
                help="Optional. Identifier of the KMS key, e.g. an ARN or key alias."
              >
                <Input
                  value={form.kms}
                  disabled={readOnly}
                  placeholder="arn:aws:kms:…"
                  onChange={(e) => setForm({ ...form, kms: e.target.value })}
                />
              </Field>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <ShieldCheck
                  className="h-4 w-4 text-fg-muted"
                  aria-hidden="true"
                />
                Placement
              </CardTitle>
            </CardHeader>
            <CardContent className="flex flex-col gap-5">
              <TokenList
                label="Storage providers"
                required
                values={form.providers}
                onChange={(providers) => setForm({ ...form, providers })}
                placeholder="wasabi"
                help="At least one provider. Data is placed only on the providers you list."
                error={providerError}
                disabled={readOnly}
              />
              <TokenList
                label="Regions"
                values={form.regions}
                onChange={(regions) => setForm({ ...form, regions })}
                placeholder="eu-central-1"
                help="Optional. Restrict placement to these provider regions."
                disabled={readOnly}
              />
              <TokenList
                label="Country residency"
                values={form.countries}
                onChange={(countries) => setForm({ ...form, countries })}
                placeholder="DE"
                help="Optional. Two-letter ISO-3166 country codes. Data stays within these countries."
                error={countryError}
                transform={(v) => v.toUpperCase()}
                disabled={readOnly}
              />
              <TokenList
                label="Storage classes"
                values={form.storageClasses}
                onChange={(storageClasses) =>
                  setForm({ ...form, storageClasses })
                }
                placeholder="standard"
                help="Optional. Allowed storage tiers."
                disabled={readOnly}
              />
              <Field
                label="Cache location hint"
                help="Optional. Preferred edge/cache location for reads."
              >
                <Input
                  value={form.cacheLocation}
                  disabled={readOnly}
                  placeholder="linode-sg"
                  onChange={(e) =>
                    setForm({ ...form, cacheLocation: e.target.value })
                  }
                />
              </Field>
            </CardContent>
          </Card>

          {!readOnly && (
            <div className="flex items-center justify-between gap-3">
              <span className="text-xs text-fg-muted">
                Active mode:{" "}
                {modeLabel(policyQuery.data?.policy.encryption.mode ?? form.mode)}
              </span>
              <div className="flex gap-2">
                <Button
                  type="button"
                  variant="outline"
                  disabled={!dirty || update.isPending}
                  onClick={() =>
                    policyQuery.data && setForm(toForm(policyQuery.data))
                  }
                >
                  Reset
                </Button>
                <Button
                  type="button"
                  disabled={!dirty || update.isPending}
                  onClick={save}
                >
                  {update.isPending ? "Saving…" : "Save policy"}
                </Button>
              </div>
            </div>
          )}
        </>
      ) : null}
    </section>
  );
}

function UpgradeNotice() {
  return (
    <Card>
      <CardContent className="flex flex-col gap-2 pt-4">
        <div className="flex items-center gap-2">
          <Lock className="h-4 w-4 text-fg-muted" aria-hidden="true" />
          <CardTitle>Available on paid plans</CardTitle>
        </div>
        <p className="text-sm text-fg-muted">
          Customising data residency — providers, country restrictions, and
          encryption mode — is available on paid plans. Your data currently uses
          the platform default. Upgrade to take control of placement.
        </p>
      </CardContent>
    </Card>
  );
}

function TokenList({
  label,
  values,
  onChange,
  placeholder,
  help,
  error,
  required,
  transform,
  disabled,
}: {
  label: string;
  values: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  help?: string;
  error?: string;
  required?: boolean;
  transform?: (v: string) => string;
  disabled?: boolean;
}) {
  const [text, setText] = useState("");
  const inputId = `token-${label.toLowerCase().replace(/\s+/g, "-")}`;
  const describedBy = error
    ? `${inputId}-error`
    : help
      ? `${inputId}-help`
      : undefined;

  function commit() {
    const raw = text.trim();
    if (!raw) return;
    const value = transform ? transform(raw) : raw;
    if (!values.includes(value)) onChange([...values, value]);
    setText("");
  }

  function onKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      commit();
    } else if (e.key === "Backspace" && !text && values.length) {
      onChange(values.slice(0, -1));
    }
  }

  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={inputId} className="text-sm font-medium text-fg">
        {label}
        {required && (
          <span aria-hidden="true" className="ms-0.5 text-danger">
            *
          </span>
        )}
      </label>
      {values.length > 0 && (
        <ul className="flex flex-wrap gap-1.5">
          {values.map((v) => (
            <li key={v}>
              <span className="inline-flex items-center gap-1 rounded-pill border border-border bg-bg-subtle px-2 py-0.5 text-xs text-fg">
                {v}
                {!disabled && (
                  <button
                    type="button"
                    aria-label={`Remove ${v}`}
                    onClick={() => onChange(values.filter((x) => x !== v))}
                    className="rounded-full text-fg-muted hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
                  >
                    <X className="h-3 w-3" aria-hidden="true" />
                  </button>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      {!disabled && (
        <Input
          id={inputId}
          value={text}
          placeholder={placeholder}
          aria-describedby={describedBy}
          aria-invalid={error ? true : undefined}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKeyDown}
          onBlur={commit}
        />
      )}
      {error ? (
        <p id={`${inputId}-error`} className="text-xs text-danger">
          {error}
        </p>
      ) : help ? (
        <p id={`${inputId}-help`} className="text-xs text-fg-muted">
          {help}
        </p>
      ) : null}
    </div>
  );
}
