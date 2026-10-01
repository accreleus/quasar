/**
 * Point the pages' root-absolute links at this build's base.
 *
 * Content links are written against the published root, `[Podman](/quasar/install/podman/)`,
 * which is right for the stable build at `/quasar/`. The edge build lives at
 * `/quasar/edge/` (src/data/docs-version.js), and without this every one of
 * those links would leave edge for stable. For a build at the root it changes
 * nothing.
 *
 * Astro 7 renders Markdown and MDX with Sätteri, so the rewrite is a Sätteri
 * hast plugin, registered on whatever processor the config ends up with by a
 * small integration (the way Starlight registers its own transforms). Only
 * `href` and `src` values that start at the root are touched: external URLs,
 * `https://accreleus.github.io/quasar/…` included, name a published location
 * on purpose and are left alone.
 *
 * Components are not Markdown. They build links from import.meta.env.BASE_URL,
 * and a test (docs-version.test.js) refuses a component that hard-codes the root.
 */

/**
 * Rewrite one URL.
 *
 * @param {string} url
 * @param {string} root  the root content is written against, e.g. `/quasar`
 * @param {string} base  this build's base, e.g. `/quasar/edge`
 */
export function rebase(url, root, base) {
	if (typeof url !== 'string' || root === base) return url;
	// Already inside this build (a link written against the edge path).
	if (url === base || url.startsWith(`${base}/`)) return url;
	if (url === root) return base;
	for (const sep of ['/', '#', '?']) {
		if (url.startsWith(`${root}${sep}`)) return `${base}${url.slice(root.length)}`;
	}
	return url;
}

/**
 * Rebase the targets of Astro's `redirects` map (targets include the root;
 * keys are already relative to the base).
 *
 * @param {Record<string, string>} redirects
 * @param {string} root
 * @param {string} base
 */
export function rebaseRedirects(redirects, root, base) {
	return Object.fromEntries(Object.entries(redirects).map(([from, to]) => [from, rebase(to, root, base)]));
}

const trim = (path) => path.replace(/\/+$/, '');

/**
 * The Sätteri hast plugin.
 *
 * @param {{ root: string, base: string }} options
 */
export function satteriBaseLinks({ root, base }) {
	const from = trim(root);
	const to = trim(base);
	return {
		name: 'quasar-base-links',
		element: {
			filter: ['a', 'img', 'source'],
			visit(node, ctx) {
				for (const name of ['href', 'src']) {
					const value = node.properties?.[name];
					if (typeof value !== 'string') continue;
					const rebased = rebase(value, from, to);
					if (rebased !== value) ctx.setProperty(node, name, rebased);
				}
			},
		},
	};
}

/**
 * Astro integration registering the plugin on the configured Markdown
 * processor (Sätteri by default; unified if the site ever switches to it).
 *
 * @param {{ root: string, base: string }} options
 * @returns {import('astro').AstroIntegration}
 */
export function baseLinks({ root, base }) {
	return {
		name: 'quasar-base-links',
		hooks: {
			'astro:config:setup': ({ config, logger }) => {
				if (trim(root) === trim(base)) return;
				const processor = /** @type {any} */ (config.markdown).processor;
				const options = processor?.options;
				if (Array.isArray(options?.hastPlugins)) {
					options.hastPlugins.push(satteriBaseLinks({ root, base }));
				} else {
					// Fail the build rather than publish edge pages linking to stable.
					throw new Error(
						`quasar-base-links: the Markdown processor (${processor?.name ?? 'none'}) takes no Sätteri hast ` +
							'plugins, so content links cannot be moved under the base. See src/data/base-links.js.',
					);
				}
				logger.info(`content links under ${trim(root)}/ are rewritten to ${trim(base)}/`);
			},
		},
	};
}
