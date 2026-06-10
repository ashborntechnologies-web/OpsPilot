"use client";

// Root-layout error boundary — must render its own <html>/<body> because it
// replaces the root layout when that layout itself throws.
export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "system-ui, sans-serif", display: "flex", minHeight: "100vh", alignItems: "center", justifyContent: "center", flexDirection: "column", gap: 12 }}>
        <h1 style={{ fontSize: 18, fontWeight: 600 }}>Something went wrong</h1>
        <p style={{ color: "#71717a", fontSize: 14 }}>{error.message || "An unexpected error occurred."}</p>
        <button
          onClick={reset}
          style={{ padding: "8px 16px", borderRadius: 6, border: "1px solid #d4d4d8", background: "#18181b", color: "white", cursor: "pointer" }}
        >
          Try again
        </button>
      </body>
    </html>
  );
}
