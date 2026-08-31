import type { ReactNode } from "react";

interface PageHeaderProps {
  title: ReactNode;
  /** subtitle line, rendered as <p className="sub"> when present */
  sub?: ReactNode;
  /** right-aligned actions, rendered in <div className="toolbar"> when present */
  actions?: ReactNode;
}

export function PageHeader({ title, sub, actions }: PageHeaderProps) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {sub && <p className="sub">{sub}</p>}
      </div>
      {actions && <div className="toolbar">{actions}</div>}
    </div>
  );
}
