// Thin launcher: the whole forge is the statically linked ./forge binary
// (linux/amd64, CGO_ENABLED=0). This file only spawns it as a child process,
// forwards the environment (MAKEFUN_ENV_URL, MAKEFUN_SHOP_TOKEN) and stdio,
// and exits with the child's exit code.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const child = spawn(path.join(dir, 'forge'), [], {
  stdio: 'inherit',
  env: process.env,
});

child.on('error', (err) => {
  console.error(`failed to start ./forge: ${err.message}`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 0);
});
