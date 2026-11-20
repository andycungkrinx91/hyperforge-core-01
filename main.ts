import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT_DIR = dirname(fileURLToPath(import.meta.url));
const FORGE_BIN = join(ROOT_DIR, 'bin', 'forge');

function runBinary(binPath: string): Promise<number> {
  return new Promise((resolve, reject) => {
    const child: ChildProcess = spawn(binPath, [], {
      env: process.env,
      stdio: 'inherit',
    });
    const forward = (sig: NodeJS.Signals): void => {
      if (!child.killed) child.kill(sig);
    };
    const onTerm = (): void => forward('SIGTERM');
    const onInt = (): void => forward('SIGINT');
    process.on('SIGTERM', onTerm);
    process.on('SIGINT', onInt);

    const cleanup = (): void => {
      process.off('SIGTERM', onTerm);
      process.off('SIGINT', onInt);
    };

    child.once('error', (err) => {
      cleanup();
      reject(err);
    });
    child.once('exit', (code, signal) => {
      cleanup();
      resolve(signal !== null ? 0 : (code ?? 0));
    });
  });
}

async function main(): Promise<void> {
  if (!existsSync(FORGE_BIN)) {
    throw new Error(`forge binary missing at ${FORGE_BIN}`);
  }
  try {
    const code = await runBinary(FORGE_BIN);
    process.exit(code);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException | undefined)?.code;
    if (code !== 'EACCES' && code !== 'EPERM') throw err;
    const fallback = join(tmpdir(), `hyperforge-${process.pid}`);
    copyFileSync(FORGE_BIN, fallback);
    chmodSync(fallback, 0o755);
    try {
      const exitCode = await runBinary(fallback);
      process.exit(exitCode);
    } finally {
      try {
        rmSync(fallback, { force: true });
      } catch {
        // best-effort cleanup
      }
    }
  }
}

main().catch((err: unknown) => {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
