import { Navbar } from "@/components/layout/navbar";

export const metadata = {
  title: "Privacy Policy — OpsPilot",
};

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2">
      <h2 className="text-base font-semibold text-zinc-900">{title}</h2>
      <div className="text-sm leading-relaxed text-zinc-600 space-y-2">{children}</div>
    </section>
  );
}

export default function PrivacyPage() {
  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-3xl mx-auto px-4 py-12">
        <h1 className="text-2xl font-bold tracking-tight">Privacy Policy</h1>
        <p className="text-sm text-muted-foreground mt-1 mb-10">Last updated: June 10, 2026</p>

        <div className="space-y-8">
          <Section title="1. What we collect">
            <ul className="list-disc pl-5 space-y-1">
              <li>
                <strong className="text-zinc-800">Account details</strong> — your email
                address, managed through our authentication provider (Clerk).
              </li>
              <li>
                <strong className="text-zinc-800">GitHub access token</strong> — stored
                encrypted (AES-256-GCM) and used only to read the repositories you
                connect and report deployment status.
              </li>
              <li>
                <strong className="text-zinc-800">AWS role ARN and external ID</strong> —
                the reference to the IAM role you created for OpsPilot. We never see or
                store your AWS credentials.
              </li>
              <li>
                <strong className="text-zinc-800">Deployment logs and events</strong> —
                build output and operational events used to show progress and diagnose
                failures.
              </li>
              <li>
                <strong className="text-zinc-800">Conversation history</strong> — the
                messages you exchange with the assistant, so your chat persists between
                sessions.
              </li>
            </ul>
          </Section>

          <Section title="2. How it's stored and protected">
            <p>
              Data lives in our managed Postgres database. GitHub tokens are encrypted
              at rest with a key derived via SHA-256 and never returned by any API.
              Secret environment variables are redacted in every list response and
              shown only through an explicit, per-value reveal action. Access to your
              AWS account happens exclusively through short-lived credentials from
              IAM role assumption with a per-tenant external ID.
            </p>
          </Section>

          <Section title="3. AI features and training">
            <p>
              When you use the chat or failure diagnosis, the relevant message or log
              excerpt is sent to our AI provider (Anthropic) to generate a response.
              We may use deployment outcomes and diagnosis feedback in{" "}
              <strong className="text-zinc-800">anonymized, aggregated form</strong> to
              improve OpsPilot&apos;s AI. You can opt out in account settings; opting
              out doesn&apos;t change the features available to you.
            </p>
          </Section>

          <Section title="4. What we don't do">
            <p>
              We do <strong className="text-zinc-800">not sell personal data</strong> —
              not to advertisers, not to data brokers, not to anyone. We don&apos;t use
              your private repository code for anything other than building and
              deploying it at your request.
            </p>
          </Section>

          <Section title="5. Deletion">
            <p>
              Deleting a project removes its deployments, logs, conversations, and
              environment variables from our database. Terminating your account deletes
              your remaining projects, your encrypted GitHub token, and your stored AWS
              role references. Infrastructure in your own AWS account is yours and is
              left untouched unless you tear it down first.
            </p>
          </Section>

          <Section title="6. Questions">
            <p>
              Privacy questions or deletion requests: contact us through the address
              listed at opspilot.dev.
            </p>
          </Section>
        </div>
      </main>
    </div>
  );
}
