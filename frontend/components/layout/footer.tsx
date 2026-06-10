import Link from "next/link";

export function Footer() {
  return (
    <footer className="border-t bg-white mt-auto">
      <div className="max-w-6xl mx-auto px-4 h-12 flex items-center justify-between text-xs text-muted-foreground">
        <span>© {new Date().getFullYear()} OpsPilot. All rights reserved.</span>
        <nav className="flex items-center gap-4">
          <Link href="/terms" className="hover:text-foreground">Terms</Link>
          <Link href="/privacy" className="hover:text-foreground">Privacy</Link>
        </nav>
      </div>
    </footer>
  );
}
