#!/bin/sh
# impeccable detect launches this as its browser. The scan runs in a throwaway
# container against a localhost mock, where Chromium's sandbox has no user
# namespaces to use; Playwright's own launches pass the same flag.
exec "$(ls -d /ms-playwright/chromium-*/chrome-linux/chrome | head -1)" --no-sandbox "$@"
