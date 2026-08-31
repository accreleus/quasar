// The name of the bench app scripts/dx/validate.sh seeds via POST /v1/apps
// when booting the LOCAL ephemeral stack (skipped entirely against a remote
// TARGET — see boot_local_stack/seed_bench_app there). Journeys that assert
// real seeded data (admin-apps, user-login-library) key off this constant;
// keep it in sync with the literal in validate.sh's seed_bench_app().
export const BENCH_APP_NAME = "validate-bench";
