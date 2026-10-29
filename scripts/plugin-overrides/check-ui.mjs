import {readdirSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {spawnSync} from 'node:child_process';

const sources = process.argv.slice(2);
if (sources.length !== 2) throw new Error('Usage: node check-ui.mjs <patched-library-source> <patched-tag-source>');
for (const source of sources) {
  const cwd = resolve(source);
  const tests = readdirSync(join(cwd, 'tests')).filter(name => name.endsWith('.test.mjs')).sort();
  if (!tests.length) throw new Error(`No regression tests in ${cwd}`);
  const result = spawnSync(process.execPath, ['--test', '--test-force-exit', ...tests.map(name => join('tests', name))], {cwd, stdio: 'inherit'});
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}
