// Deliberately blank landing page for mercmerc.app / mercmerc.net.
//
// A holding page, not a product surface: no analytics, no external fetches, no
// fonts, nothing that phones anywhere. It answers 200 with a black screen and
// says so in a comment a curious visitor can read via view-source, because an
// unexplained black page reads as a broken deploy rather than an intentional one.
//
// robots noindex is deliberate too -- a placeholder that gets indexed is a
// placeholder that outranks the real site later.
const PAGE = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>merc</title>
<style>
  html,body{margin:0;padding:0;height:100%;background:#000;}
</style>
</head>
<body>
<!-- Intentionally blank. This domain is reserved; nothing is served here yet. -->
</body>
</html>
`;

export default {
  async fetch(request) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("method not allowed", { status: 405, headers: { allow: "GET, HEAD" } });
    }
    return new Response(request.method === "HEAD" ? null : PAGE, {
      status: 200,
      headers: {
        "content-type": "text/html; charset=utf-8",
        "cache-control": "public, max-age=300",
        "x-content-type-options": "nosniff",
        "referrer-policy": "no-referrer",
        // No framing: a blank page is a tempting clickjack backdrop.
        "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'",
      },
    });
  },
};
