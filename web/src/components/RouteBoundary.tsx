import { Component, type ErrorInfo, type ReactNode } from "react";

export function RouteSkeleton({ label = "Loading page" }: { label?: string }) {
  return (
    <div className="route-skeleton" role="status" aria-live="polite" aria-label={label}>
      <span className="route-skeleton__title" />
      <span className="route-skeleton__line" />
      <span className="route-skeleton__line route-skeleton__line--short" />
      <div className="route-skeleton__cards">
        <span />
        <span />
        <span />
      </div>
      <span className="sr-only">{label}…</span>
    </div>
  );
}

interface BoundaryProps {
  children: ReactNode;
}

interface BoundaryState {
  error: Error | null;
}

/** Keeps render/lazy failures actionable instead of replacing the route with blank DOM. */
export class RouteErrorBoundary extends Component<BoundaryProps, BoundaryState> {
  state: BoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): BoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("[quasar] route render failed", error, info.componentStack);
  }

  private retry = () => this.setState({ error: null });

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <main className="route-error" role="alert">
        <p className="eyebrow">Page unavailable</p>
        <h1>This view could not be rendered</h1>
        <p>
          Your session is still safe. Retry the view, or reload if an updated application chunk
          failed to download.
        </p>
        <details>
          <summary>Technical detail</summary>
          <code>{this.state.error.message || this.state.error.name}</code>
        </details>
        <div className="route-error__actions">
          <button className="btn btn-primary" type="button" onClick={this.retry}>Try again</button>
          <button className="btn btn-ghost" type="button" onClick={() => window.location.reload()}>
            Reload application
          </button>
        </div>
      </main>
    );
  }
}
