import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const ROOT_DIR = dirname(fileURLToPath(import.meta.url));
const FORGE_BIN = join(ROOT_DIR, 'bin', 'forge');

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 50;
const RETRY_MAX_MS = 1_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 6));
}

function startForgeBinary(): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(FORGE_BIN, [], {
      env: process.env,
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    let resolved = false;
    let buf = '';

    child.once('error', (err) => {
      if (!resolved) {
        resolved = true;
        reject(err);
      }
    });

    child.once('exit', (code, signal) => {
      if (!resolved) {
        resolved = true;
        reject(new Error(`forge binary exited before reporting port (code=${code}, signal=${signal})`));
      }
    });

    child.stdout?.on('data', (chunk: Buffer) => {
      if (resolved) return;
      buf += chunk.toString('utf8');
      const nl = buf.indexOf('\n');
      if (nl === -1) return;
      const line = buf.slice(0, nl).trim();
      const match = /^PORT=(\d+)$/.exec(line);
      if (match) {
        resolved = true;
        resolve({ child, port: Number(match[1]) });
      }
    });
  });
}

async function registerPort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, {
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // retry until env signals done
    }
    await sleep(retryDelay(attempt));
  }
}

async function main(): Promise<void> {
  if (ENV_URL === '') {
    process.stderr.write('[entry] MAKEFUN_ENV_URL is not set\n');
    process.exit(1);
  }

  const { child, port } = await startForgeBinary();
  const stopChild = (): void => {
    if (!child.killed) child.kill('SIGTERM');
  };
  process.once('SIGTERM', () => {
    stopChild();
    process.exit(0);
  });
  process.once('SIGINT', () => {
    stopChild();
    process.exit(0);
  });

  process.stdout.write(`[entry] forge listening on port ${port}\n`);
  await registerPort(port);
  process.stdout.write('[entry] registered; waiting for done\n');
  await waitForDone();
  stopChild();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
