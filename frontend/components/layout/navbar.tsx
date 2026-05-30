"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { UserButton } from "@clerk/nextjs";
import { Rocket } from "lucide-react";
import { cn } from "@/lib/utils";

export function Navbar() {
  const path = usePathname();

  return (
    <header className="border-b bg-white">
      <div className="max-w-6xl mx-auto px-4 h-14 flex items-center justify-between">
        <Link href="/projects" className="flex items-center gap-2 font-semibold text-sm">
          <Rocket className="h-4 w-4 text-indigo-600" />
          ConvDeploy
        </Link>

        <nav className="flex items-center gap-6 text-sm text-muted-foreground">
          <Link
            href="/projects"
            className={cn(path.startsWith("/projects") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            Projects
          </Link>
          <Link
            href="/aws-accounts"
            className={cn(path.startsWith("/aws-accounts") ? "text-foreground font-medium" : "hover:text-foreground")}
          >
            AWS Accounts
          </Link>
        </nav>

        <UserButton />
      </div>
    </header>
  );
}
