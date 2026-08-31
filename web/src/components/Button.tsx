import type { ButtonHTMLAttributes, ReactNode } from "react";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
export type ButtonSize = "lg" | "md" | "sm";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** Icon-only button — renders as a square with no padding */
  iconOnly?: boolean;
  children?: ReactNode;
}

export function Button({
  variant = "secondary",
  size = "md",
  iconOnly = false,
  className,
  ...rest
}: ButtonProps) {
  const classes = [
    "btn",
    variant === "primary" ? "btn-primary"
      : variant === "ghost" ? "btn-ghost"
      : variant === "danger" ? "btn-danger"
      : /* secondary */ "",
    size === "lg" ? "btn-lg"
      : size === "sm" ? "btn-sm"
      : "",
    iconOnly ? "btn-icon" : "",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");

  return <button className={classes} {...rest} />;
}
