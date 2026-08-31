import type { InputHTMLAttributes, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";

import { IconSearch } from "./icons";

/* ------------------------------------------------------------------ */
/* Field wrapper (label + hint)                                         */
/* ------------------------------------------------------------------ */

interface FieldProps {
  label: string;
  hint?: string;
  id?: string;
  children: React.ReactNode;
}

export function Field({ label, hint, id, children }: FieldProps) {
  return (
    <div className="field">
      <label className="label" htmlFor={id}>
        {label}
      </label>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* TextField — text / number / email / password input inside a Field    */
/* ------------------------------------------------------------------ */

interface TextFieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string;
  hint?: string;
  mono?: boolean;
}

export function TextField({ label, hint, id, mono, className, ...rest }: TextFieldProps) {
  const fieldId = id ?? rest.name ?? label.toLowerCase().replace(/\s+/g, "-");
  return (
    <Field label={label} hint={hint} id={fieldId}>
      <input
        id={fieldId}
        className={["input", mono ? "mono" : "", className ?? ""].filter(Boolean).join(" ")}
        {...rest}
      />
    </Field>
  );
}

/* ------------------------------------------------------------------ */
/* SelectField                                                          */
/* ------------------------------------------------------------------ */

interface SelectFieldProps extends SelectHTMLAttributes<HTMLSelectElement> {
  label: string;
  hint?: string;
  children: React.ReactNode;
}

export function SelectField({ label, hint, id, className, children, ...rest }: SelectFieldProps) {
  const fieldId = id ?? rest.name ?? label.toLowerCase().replace(/\s+/g, "-");
  return (
    <Field label={label} hint={hint} id={fieldId}>
      <select id={fieldId} className={["select", className ?? ""].filter(Boolean).join(" ")} {...rest}>
        {children}
      </select>
    </Field>
  );
}

/* ------------------------------------------------------------------ */
/* TextareaField                                                        */
/* ------------------------------------------------------------------ */

interface TextareaFieldProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  label: string;
  hint?: string;
}

export function TextareaField({ label, hint, id, className, ...rest }: TextareaFieldProps) {
  const fieldId = id ?? rest.name ?? label.toLowerCase().replace(/\s+/g, "-");
  return (
    <Field label={label} hint={hint} id={fieldId}>
      <textarea
        id={fieldId}
        className={["input", className ?? ""].filter(Boolean).join(" ")}
        {...rest}
      />
    </Field>
  );
}

/* ------------------------------------------------------------------ */
/* Switch                                                               */
/* ------------------------------------------------------------------ */

interface SwitchProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label?: string;
  id?: string;
  disabled?: boolean;
  /** Accessible name for a switch with no visible `label` (a trailing,
   *  bare toggle). Ignored when `label` is set — that already names it. */
  "aria-label"?: string;
}

export function Switch({ checked, onChange, label, id, disabled, "aria-label": ariaLabel }: SwitchProps) {
  const switchId = id ?? (label ? label.toLowerCase().replace(/\s+/g, "-") : undefined);
  return (
    <label className="switch-row" style={{ display: "flex", alignItems: "center", gap: "var(--s3)", cursor: disabled ? "not-allowed" : "pointer" }}>
      <label className="switch">
        <input
          id={switchId}
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          disabled={disabled}
          aria-label={label ? undefined : ariaLabel}
        />
        <span className="track" />
        <span className="thumb" />
      </label>
      {label && <span style={{ fontSize: "var(--t-sm)", color: disabled ? "var(--text-4)" : "var(--text-2)" }}>{label}</span>}
    </label>
  );
}

/* ------------------------------------------------------------------ */
/* Checkbox                                                             */
/* ------------------------------------------------------------------ */

interface CheckboxProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label: string;
  id?: string;
  disabled?: boolean;
}

export function Checkbox({ checked, onChange, label, id, disabled }: CheckboxProps) {
  const checkId = id ?? label.toLowerCase().replace(/\s+/g, "-");
  return (
    <label className="check" htmlFor={checkId} style={{ opacity: disabled ? 0.5 : 1, cursor: disabled ? "not-allowed" : "pointer" }}>
      <input
        id={checkId}
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        disabled={disabled}
      />
      {" "}{label}
    </label>
  );
}

/* ------------------------------------------------------------------ */
/* SearchInput                                                          */
/* ------------------------------------------------------------------ */

interface SearchInputProps extends InputHTMLAttributes<HTMLInputElement> {
  /** override default "Search…" placeholder */
  placeholder?: string;
}

export function SearchInput({ className, ...rest }: SearchInputProps) {
  return (
    <div className={["search", className ?? ""].filter(Boolean).join(" ")}>
      <IconSearch />
      <input placeholder={rest.placeholder ?? "Search…"} {...rest} />
    </div>
  );
}
