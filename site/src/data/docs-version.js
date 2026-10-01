/**
 * Which version of the documentation a build is.
 *
 * The site is published as two builds in one GitHub Pages artifact
 * (.github/workflows/pages.yml):
 *
 *   stable  <root>/        built from the latest release tag
 *   edge    <root>/edge/   built from `develop`
 *
 * The workflow says which one it is building through the environment, so the
 * same astro.config.mjs builds either:
 *
 *   QUASAR_DOCS_CHANNEL       `stable` or `edge`. Unset means a local build: no
 *                             version switcher, published at the stable root.
 *   QUASAR_DOCS_BASE          the base path, if not the channel's default
 *                             (`/quasar` for stable, `/quasar/edge` for edge).
 *   QUASAR_DOCS_STABLE_LABEL  the release the stable build is, e.g. `v0.3.0`;
 *                             shown in the switcher on both builds.
 *
 * Node-only (reads process.env): imported by astro.config.mjs and by the
 * switcher's frontmatter, never by a client script.
 */

/** The published root. The product UI ships this URL to operators, so it is
 * not free to change; see the header of .github/workflows/pages.yml. */
export const DOCS_ROOT = '/quasar';

/** Where each channel lives, relative to the host. */
export const CHANNEL_BASES = {
	stable: DOCS_ROOT,
	edge: `${DOCS_ROOT}/edge`,
};

/** Strip trailing slashes and make sure there is one leading slash. */
function normaliseBase(value) {
	const trimmed = value.trim().replace(/\/+$/, '');
	return trimmed.startsWith('/') ? trimmed : `/${trimmed}`;
}

/**
 * @param {Record<string, string | undefined>} env
 * @returns {{ channel: 'stable' | 'edge', base: string, switcher: boolean, stableLabel: string }}
 */
export function docsVersion(env = process.env) {
	const raw = (env.QUASAR_DOCS_CHANNEL ?? '').trim();
	if (raw !== '' && raw !== 'stable' && raw !== 'edge') {
		throw new Error(`QUASAR_DOCS_CHANNEL must be "stable" or "edge", not "${raw}".`);
	}
	/** @type {'stable' | 'edge'} */
	const channel = raw === 'edge' ? 'edge' : 'stable';
	const base = env.QUASAR_DOCS_BASE ? normaliseBase(env.QUASAR_DOCS_BASE) : CHANNEL_BASES[channel];
	return {
		channel,
		base,
		// Only a build that knows which version it is can offer the other one.
		switcher: raw !== '',
		stableLabel: (env.QUASAR_DOCS_STABLE_LABEL ?? '').trim(),
	};
}

/**
 * The same page in the other channel: `pathname` with this build's base
 * replaced by the target channel's. A path outside the base maps to the
 * target's home.
 *
 * @param {string} pathname  the current page, e.g. `/quasar/edge/install/podman/`
 * @param {string} fromBase  this build's base, e.g. `/quasar/edge`
 * @param {string} toBase    the target channel's base, e.g. `/quasar`
 */
export function counterpartPath(pathname, fromBase, toBase) {
	const from = normaliseBase(fromBase);
	const to = normaliseBase(toBase);
	if (pathname === from || pathname === `${from}/`) return `${to}/`;
	if (!pathname.startsWith(`${from}/`)) return `${to}/`;
	return `${to}${pathname.slice(from.length)}`;
}
