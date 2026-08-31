import { Link } from "react-router-dom";

export function NotFound() {
  return (
    <div className="centered-screen">
      <section className="card card-pad">
        <div className="empty">
          <h3>404</h3>
          <p>That page doesn’t exist.</p>
          <Link to="/app" className="btn btn-primary">
            Go to the app
          </Link>
        </div>
      </section>
    </div>
  );
}
