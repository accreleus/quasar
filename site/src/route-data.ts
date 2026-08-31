import { defineRouteMiddleware } from '@astrojs/starlight/route-data';

/**
 * Two small corrections to the pages starlight-openapi generates from
 * `protocol/openapi.yaml`.
 *
 * 1. The plugin hard-codes the title "Overview" for both its schema landing
 *    page and every per-tag landing page, and exposes no option to change it.
 *    On this site that produced fourteen pages sharing one title — in the
 *    browser tab, in the sidebar's pagination, and, worst, as fourteen
 *    identical "Overview" hits in Pagefind search. Titles are derived from the
 *    URL here rather than patched into the plugin. Starlight builds the
 *    document `<title>` and the Open Graph titles into `head` before route
 *    middleware runs, so those are rewritten separately from the heading —
 *    setting `entry.data.title` alone fixes the `<h1>` and nothing else.
 *
 * 2. Operation titles come from the spec's `summary`, which in a frozen
 *    contract is written as a sentence of prose, not as a heading: half of
 *    ours run past sixty characters. They are worth keeping — they say what
 *    the endpoint is for, and the method and path are already shown directly
 *    beneath — but they should not be set at billboard size. A marker in the
 *    document head lets `theme.css` scale the heading down for these pages
 *    only, without the spec or the plugin having to change.
 *
 * It also keeps the 141 operation pages out of the site search. They are short,
 * repetitive, and titled with the spec's own wording, which made them outrank
 * the operator guides they share vocabulary with: before this, searching
 * "encoder" returned the encoder-certification endpoint rather than the
 * Encoders tuning page, and "invites" returned POST /v1/auth/register rather
 * than Users and invites. The reference's own landing pages stay indexed, so
 * the reference is still findable by search; from there it is navigated by tag
 * in the sidebar, or by the full path list on its landing page.
 *
 * The spec is a frozen interface, so nothing here writes back to it.
 */

/** Where the plugin is mounted, relative to the site base path. Must match the
 * `base` given to starlightOpenAPI() in astro.config.mjs. */
const API_ROOT = ['developer', 'api'];

/**
 * Titles for the tag landing pages. A tag missing from this map falls back to
 * its capitalised name, so adding a tag to the spec can never produce a broken
 * page here — only a slightly plainer title.
 */
const TAG_TITLES: Record<string, string> = {
	access: 'Remote access and TLS',
	admin: 'Admin oversight',
	agent: 'Node-agent internal',
	apps: 'App library',
	auth: 'Authentication',
	dev: 'Development-only',
	hosts: 'Hosts and GPUs',
	images: 'App images',
	me: 'The current user',
	sessions: 'Sessions',
	setup: 'First-run setup',
	signaling: 'Signaling',
	users: 'User administration',
};

function capitalise(value: string): string {
	return value.charAt(0).toUpperCase() + value.slice(1);
}

/**
 * Return the path relative to the API root, or undefined when the page is not
 * part of the generated reference. Works regardless of the site's base path.
 */
function apiSubPath(pathname: string): string | undefined {
	const segments = pathname.split('/').filter(Boolean);
	const start = segments.indexOf(API_ROOT[0]!);
	if (start === -1) return undefined;
	if (!API_ROOT.every((segment, i) => segments[start + i] === segment)) return undefined;
	return segments.slice(start + API_ROOT.length).join('/');
}

/** The replacement title for a generated page, or undefined to keep the one
 * the plugin chose (which is right for operation pages: their title is the
 * spec's summary). */
function retitle(subPath: string): string | undefined {
	if (subPath === '') return 'Control-plane API';
	const tag = /^operations\/tags\/(?<tag>[^/]+)$/.exec(subPath)?.groups?.['tag'];
	return tag ? (TAG_TITLES[tag] ?? capitalise(tag)) : undefined;
}

export const onRequest = defineRouteMiddleware((context) => {
	const { starlightRoute } = context.locals;
	const subPath = apiSubPath(context.url.pathname);
	if (subPath === undefined) return;

	const title = retitle(subPath);

	// Landing pages (the schema root and one per tag) are exactly the pages
	// retitle() handles, so the same test decides what stays searchable.
	starlightRoute.entry.data.pagefind = title !== undefined;

	if (title !== undefined) {
		const previous = starlightRoute.entry.data.title;
		starlightRoute.entry.data.title = title;

		for (const entry of starlightRoute.head) {
			if (entry.tag === 'title' && typeof entry.content === 'string') {
				entry.content = entry.content.replace(previous, title);
			} else if (
				entry.tag === 'meta' &&
				(entry.attrs?.['property'] === 'og:title' || entry.attrs?.['name'] === 'twitter:title') &&
				entry.attrs['content'] === previous
			) {
				entry.attrs['content'] = title;
			}
		}
	}

	// The hook theme.css scales the page heading with. Marking every page in
	// the reference, not just operation pages, keeps the rule a single
	// selector; the landing pages have short titles either way.
	starlightRoute.head.push({
		tag: 'meta',
		attrs: { name: 'quasar:page-kind', content: 'api-reference' },
	});
});
