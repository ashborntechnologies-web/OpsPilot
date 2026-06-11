import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import { ClerkProvider } from "@clerk/nextjs";
import { Toaster } from "@/components/ui/sonner";
import { Footer } from "@/components/layout/footer";
import "./globals.css";

const geistSans = Geist({ variable: "--font-geist-sans", subsets: ["latin"] });
const geistMono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  title: "OpsPilot — AI DevOps Assistant",
  description:
    "Monitor, deploy, and operate your AWS infrastructure through conversation. 24/7 AI watching your production.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <ClerkProvider afterSignOutUrl="/sign-in">
      <html
        lang="en"
        className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}
      >
        <body className="min-h-screen flex flex-col bg-background text-foreground">
          <div className="flex-1 flex flex-col">{children}</div>
          <Footer />
          <Toaster position="bottom-right" />
        </body>
      </html>
    </ClerkProvider>
  );
}
