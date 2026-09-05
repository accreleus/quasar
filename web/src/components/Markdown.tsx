/**
 * Release notes come off a third party's GitHub Release body, so they are
 * UNTRUSTED TEXT rendered inside an admin's session. Two layers, neither of
 * which may be removed: react-markdown builds React elements rather than
 * setting innerHTML and ignores raw HTML unless `rehype-raw` is added (it is
 * not), and rehype-sanitize strips anything a future plugin might let through.
 *
 * Guarded by Markdown.test.tsx, which asserts a <script> in a body renders as
 * text and never as a script element.
 */

import ReactMarkdown from "react-markdown";
import rehypeSanitize from "rehype-sanitize";

export function Markdown({ children }: { children: string }) {
  return (
    <div className="release-notes">
      <ReactMarkdown
        rehypePlugins={[rehypeSanitize]}
        components={{
          // Upstream links open away from the console, and never carry the
          // referrer or a window handle back to it.
          a: ({ ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
