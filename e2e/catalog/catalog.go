// Package catalog defines the set of test applications used by the E2E framework.
// Each entry specifies the framework, files to push to GitHub, and expected behaviour.
package catalog

// TestApp defines one application in the E2E test catalog.
type TestApp struct {
	// RepoName is the GitHub repository name (created under E2E_GITHUB_OWNER).
	RepoName string
	// Framework matches OpsPilot's framework detection values.
	Framework string
	// Description is pushed to GitHub and shown in reports.
	Description string
	// Files is the complete file tree to commit. Keys are relative file paths.
	Files map[string]string
	// HealthPath is the HTTP path that should return 200 when the app is live.
	HealthPath string
	// ExpectSuccess is false for intentionally broken apps.
	ExpectSuccess bool
	// ExpectedFailureKeyword is checked in the failure_reason for broken apps.
	ExpectedFailureKeyword string
	// StartCommand overrides the Dockerfile CMD (leave empty to use Dockerfile CMD).
	StartCommand string
	// EnvVars are set on the OpsPilot environment before the first deploy.
	EnvVars map[string]string
	// BrokenVariant is an alternate file set that causes a runtime crash —
	// used by rollback tests to deploy a bad version after a good one.
	BrokenVariant map[string]string
}

// Catalog is the authoritative list of test apps.
var Catalog = map[string]*TestApp{
	"nodejs-express":    NodejsExpress,
	"fastapi":           FastAPI,
	"flask":             Flask,
	"nextjs":            Nextjs,
	"broken-build":      BrokenBuild,
	"broken-runtime":    BrokenRuntime,
	"broken-missing-env": BrokenMissingEnv,
}

// ── Node.js / Express ─────────────────────────────────────────────────────────

var NodejsExpress = &TestApp{
	RepoName:      "e2e-nodejs-express",
	Framework:     "express",
	Description:   "Minimal Express server — OpsPilot E2E test app",
	ExpectSuccess: true,
	HealthPath:    "/health",
	Files: map[string]string{
		"app.js": `const express = require('express');
const app = express();
const port = process.env.PORT || 3000;
app.get('/health', (_req, res) => res.json({ status: 'ok', framework: 'express' }));
app.get('/', (_req, res) => res.json({ hello: 'world' }));
app.listen(port, () => console.log('listening on', port));
`,
		"package.json": `{
  "name": "e2e-express",
  "version": "1.0.0",
  "main": "app.js",
  "scripts": { "start": "node app.js" },
  "dependencies": { "express": "^4.18.2" }
}
`,
		"Dockerfile": `FROM node:20-alpine
WORKDIR /app
COPY package.json .
RUN npm install --production
COPY . .
EXPOSE 3000
CMD ["node", "app.js"]
`,
	},
	BrokenVariant: map[string]string{
		"app.js": `// runtime crash — exits immediately
console.error("fatal: misconfiguration");
process.exit(1);
`,
		"package.json": `{"name":"e2e-express","version":"2.0.0","main":"app.js"}`,
		"Dockerfile": `FROM node:20-alpine
WORKDIR /app
COPY . .
CMD ["node", "app.js"]
`,
	},
}

// ── FastAPI ───────────────────────────────────────────────────────────────────

var FastAPI = &TestApp{
	RepoName:      "e2e-fastapi",
	Framework:     "fastapi",
	Description:   "Minimal FastAPI server — OpsPilot E2E test app",
	ExpectSuccess: true,
	HealthPath:    "/health",
	Files: map[string]string{
		"main.py": `import os
from fastapi import FastAPI

app = FastAPI()

@app.get("/health")
def health():
    return {"status": "ok", "framework": "fastapi"}

@app.get("/")
def root():
    return {"hello": "world"}
`,
		"requirements.txt": `fastapi==0.115.0
uvicorn[standard]==0.32.0
`,
		"Dockerfile": `FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PORT=8000
EXPOSE 8000
CMD ["sh", "-c", "uvicorn main:app --host 0.0.0.0 --port ${PORT:-8000}"]
`,
	},
	BrokenVariant: map[string]string{
		"main.py": `# runtime crash
raise RuntimeError("intentional crash for rollback test")
`,
		"requirements.txt": `fastapi==0.115.0
uvicorn[standard]==0.32.0
`,
		"Dockerfile": `FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
CMD ["python", "main.py"]
`,
	},
}

// ── Flask ─────────────────────────────────────────────────────────────────────

var Flask = &TestApp{
	RepoName:      "e2e-flask",
	Framework:     "flask",
	Description:   "Minimal Flask server — OpsPilot E2E test app",
	ExpectSuccess: true,
	HealthPath:    "/health",
	Files: map[string]string{
		"app.py": `import os
from flask import Flask, jsonify

app = Flask(__name__)

@app.route("/health")
def health():
    return jsonify({"status": "ok", "framework": "flask"})

@app.route("/")
def index():
    return jsonify({"hello": "world"})

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", 5000)))
`,
		"requirements.txt": `flask==3.0.3
gunicorn==22.0.0
`,
		"Dockerfile": `FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PORT=5000
EXPOSE 5000
CMD ["sh", "-c", "gunicorn app:app --bind 0.0.0.0:${PORT:-5000}"]
`,
	},
}

// ── Next.js ───────────────────────────────────────────────────────────────────

var Nextjs = &TestApp{
	RepoName:      "e2e-nextjs",
	Framework:     "nextjs",
	Description:   "Minimal Next.js app — OpsPilot E2E test app",
	ExpectSuccess: true,
	HealthPath:    "/api/health",
	Files: map[string]string{
		"package.json": `{
  "name": "e2e-nextjs",
  "version": "1.0.0",
  "scripts": {
    "dev": "next dev",
    "build": "next build",
    "start": "next start -p ${PORT:-3000}"
  },
  "dependencies": {
    "next": "14.2.5",
    "react": "^18",
    "react-dom": "^18"
  }
}
`,
		"next.config.js": `/** @type {import('next').NextConfig} */
module.exports = { output: 'standalone' };
`,
		"app/page.js": `export default function Home() {
  return <main><h1>Hello from OpsPilot E2E</h1></main>;
}
`,
		"app/api/health/route.js": `import { NextResponse } from 'next/server';
export function GET() {
  return NextResponse.json({ status: 'ok', framework: 'nextjs' });
}
`,
		"Dockerfile": `FROM node:20-alpine AS builder
WORKDIR /app
COPY package.json .
RUN npm install
COPY . .
RUN npm run build

FROM node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/public ./public
EXPOSE 3000
CMD ["node", "server.js"]
`,
	},
}

// ── Broken: build failure ─────────────────────────────────────────────────────

var BrokenBuild = &TestApp{
	RepoName:               "e2e-broken-build",
	Framework:              "nodejs",
	Description:            "Intentionally broken Dockerfile — OpsPilot E2E failure test",
	ExpectSuccess:          false,
	ExpectedFailureKeyword: "build failed",
	Files: map[string]string{
		"app.js":       `console.log("this will never run");`,
		"package.json": `{"name":"broken","version":"1.0.0"}`,
		"Dockerfile": `FROM node:20-alpine
RUN this_command_does_not_exist_intentionally
COPY . .
CMD ["node", "app.js"]
`,
	},
}

// ── Broken: runtime crash ─────────────────────────────────────────────────────

var BrokenRuntime = &TestApp{
	RepoName:               "e2e-broken-runtime",
	Framework:              "nodejs",
	Description:            "App that exits immediately at startup — OpsPilot E2E failure test",
	ExpectSuccess:          false,
	ExpectedFailureKeyword: "stabilize",
	Files: map[string]string{
		"app.js": `console.error("FATAL: intentional runtime crash");
process.exit(1);
`,
		"package.json": `{"name":"broken-runtime","version":"1.0.0","main":"app.js"}`,
		"Dockerfile": `FROM node:20-alpine
WORKDIR /app
COPY . .
CMD ["node", "app.js"]
`,
	},
}

// ── Broken: missing required env var ─────────────────────────────────────────

var BrokenMissingEnv = &TestApp{
	RepoName:               "e2e-broken-missing-env",
	Framework:              "nodejs",
	Description:            "App that crashes without a required env var — OpsPilot E2E failure test",
	ExpectSuccess:          false,
	ExpectedFailureKeyword: "stabilize",
	Files: map[string]string{
		"app.js": `const express = require('express');
const requiredSecret = process.env.REQUIRED_SECRET;
if (!requiredSecret) {
  console.error("FATAL: REQUIRED_SECRET env var not set");
  process.exit(1);
}
const app = express();
app.get('/health', (_, res) => res.json({ ok: true }));
app.listen(3000);
`,
		"package.json": `{
  "name":"broken-missing-env","version":"1.0.0",
  "dependencies":{"express":"^4.18.2"}
}`,
		"Dockerfile": `FROM node:20-alpine
WORKDIR /app
COPY package.json .
RUN npm install --production
COPY . .
CMD ["node", "app.js"]
`,
	},
}
