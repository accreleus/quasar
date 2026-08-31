/**
 * Card / Panel primitives.
 * `.card`  — standard elevated card surface
 * `.panel` — flat panel / sidebar section
 */
import type { HTMLAttributes, ReactNode } from "react";

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  children: ReactNode;
}

export function Card({ className, children, ...rest }: CardProps) {
  return (
    <div className={["card", className ?? ""].filter(Boolean).join(" ")} {...rest}>
      {children}
    </div>
  );
}

export function Panel({ className, children, ...rest }: CardProps) {
  return (
    <div className={["panel", className ?? ""].filter(Boolean).join(" ")} {...rest}>
      {children}
    </div>
  );
}
