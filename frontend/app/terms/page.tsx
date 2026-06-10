import { Navbar } from "@/components/layout/navbar";

export const metadata = {
  title: "Terms of Service — OpsPilot",
};

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2">
      <h2 className="text-base font-semibold text-zinc-900">{title}</h2>
      <div className="text-sm leading-relaxed text-zinc-600 space-y-2">{children}</div>
    </section>
  );
}

export default function TermsPage() {
  return (
    <div className="min-h-screen bg-zinc-50">
      <Navbar />
      <main className="max-w-3xl mx-auto px-4 py-12">
        <h1 className="text-2xl font-bold tracking-tight">Terms of Service</h1>
        <p className="text-sm text-muted-foreground mt-1 mb-10">Last updated: June 10, 2026</p>

        <div className="space-y-8">
          <Section title="1. What OpsPilot is">
            <p>
              OpsPilot is a conversational deployment platform. You connect a GitHub
              repository and your own AWS account, and OpsPilot builds, deploys, and
              operates your application there. By creating an account you agree to
              these terms.
            </p>
          </Section>

          <Section title="2. Our intellectual property">
            <p>
              The AI prompts, models, classification logic, and training data that power
              OpsPilot&apos;s conversational interface and failure diagnosis are{" "}
              <strong className="text-zinc-800">proprietary trade secrets</strong> of
              OpsPilot. The structure and wording of AI responses are part of that
              protected work.
            </p>
            <p>You may not:</p>
            <ul className="list-disc pl-5 space-y-1">
              <li>use OpsPilot to build, train, or improve a competing deployment platform;</li>
              <li>scrape, systematically collect, or reverse engineer AI outputs;</li>
              <li>probe the service to reconstruct our prompts or models;</li>
              <li>use the service for competitive intelligence gathering.</li>
            </ul>
          </Section>

          <Section title="3. Your infrastructure stays yours">
            <p>
              OpsPilot follows a bring-your-own-cloud (BYOC) model. Everything deployed —
              containers, load balancers, build pipelines — runs in{" "}
              <strong className="text-zinc-800">your</strong> AWS account and belongs to
              you. OpsPilot operates through a scoped IAM role you can revoke at any
              time, and tags every resource it creates so you can always identify them.
            </p>
          </Section>

          <Section title="4. How we use your data to improve the service">
            <p>
              We may use deployment outcomes, conversation patterns, and diagnosis
              feedback in <strong className="text-zinc-800">anonymized, aggregated
              form</strong> to improve OpsPilot — for example, to make failure diagnosis
              more accurate. You can opt out of this in your account settings at any
              time, and opting out never reduces the service you receive.
            </p>
          </Section>

          <Section title="5. Acceptable use">
            <p>
              Don&apos;t use OpsPilot to deploy anything illegal, to attack third
              parties, or to circumvent the usage limits of the plan you&apos;re on.
              We may suspend accounts that put other users or the platform at risk.
            </p>
          </Section>

          <Section title="6. Limitation of liability">
            <p>
              OpsPilot orchestrates infrastructure in your AWS account, but that
              infrastructure is operated by AWS and configured at your direction. To the
              maximum extent permitted by law, OpsPilot is not liable for outages, data
              loss, or costs arising from failures of infrastructure in your AWS account,
              from your application&apos;s own behavior, or from AWS service
              interruptions. Our total liability is limited to the fees you paid us in
              the twelve months before the claim.
            </p>
          </Section>

          <Section title="7. Termination">
            <p>
              You can stop using OpsPilot at any time. Deleting your account removes
              your projects, conversation history, and stored credentials from our
              systems (see the Privacy Policy). Resources in your AWS account are yours
              and are not deleted unless you ask OpsPilot to tear them down first.
            </p>
          </Section>

          <Section title="8. Changes">
            <p>
              We may update these terms as the product evolves. Material changes will be
              announced in the product before they take effect.
            </p>
          </Section>
        </div>
      </main>
    </div>
  );
}
