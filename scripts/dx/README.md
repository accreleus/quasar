# scripts/dx — the Makefile's orchestration layer

Every `make` target's logic lives here: guards, instance isolation, RESULT lines, redaction. The Makefile is a thin façade over these. Catalogue: `docs/developer-tooling.md`.

**A caller-settable value never goes on a Makefile recipe line** (#550). Make expands `$(ARGS)` into the recipe's command *text*, which bash then parses, so a `;` in the value is a second command — before any script here can check it, and quoting it does not help. Knobs travel by environment instead; the receiving script calls `dx_env_argv` (`common.sh`) to split and shape-check them back into arguments. When you add a target, give the script the knob's *name*, not its value: `@bash $(DX)/foo.sh`, never `@bash $(DX)/foo.sh $(ARGS)`. `scripts/dx/tests/run.sh` fails the build if a recipe line grows one.
