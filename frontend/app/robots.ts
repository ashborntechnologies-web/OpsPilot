import type { MetadataRoute } from "next";

// robots.txt for the frontend. The marketing landing and legal pages are crawlable;
// the authenticated application surface (everything behind Clerk auth) is disallowed —
// those routes hold tenant data and have no SEO value. Mirrors the API host's robots.txt.
export default function robots(): MetadataRoute.Robots {
  const base = process.env.NEXT_PUBLIC_SITE_URL?.replace(/\/$/, "");
  return {
    rules: {
      userAgent: "*",
      allow: ["/", "/privacy", "/terms"],
      disallow: [
        "/projects",
        "/orgs",
        "/aws-accounts",
        "/incidents",
        "/postmortems",
        "/settings",
        "/invites",
        "/sign-in",
        "/sign-up",
      ],
    },
    ...(base ? { sitemap: `${base}/sitemap.xml` } : {}),
  };
}
