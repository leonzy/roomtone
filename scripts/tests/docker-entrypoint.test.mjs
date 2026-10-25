import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const entrypoint = readFileSync(new URL("../docker-entrypoint.sh", import.meta.url), "utf8");

function binary(version, buildType = "full", buildTime = "2026-10-19_00:00:00") {
  return `#!/bin/sh
if [ "$1" = "-version" ]; then
  echo "Roomtone Version: ${version}"
  echo "Build Type: ${buildType}"
  echo "Build Time: ${buildTime}"
else
  echo "RUNNING ${version} ${buildType}"
fi
`;
}

function runEntrypoint(t, target, source = binary("2.13.3")) {
  const root = mkdtempSync(join(tmpdir(), "songloft-entrypoint-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "data"));
  mkdirSync(join(root, "music"));
  // BINARY_SOURCE 在镜像内是 /app/roomtone，replaceAll("/app", root) 后即 <root>/roomtone
  writeFileSync(join(root, "roomtone"), source, { mode: 0o755 });
  if (target !== null) writeFileSync(join(root, "data/songloft"), target, { mode: 0o755 });
  const script = join(root, "entrypoint.sh");
  writeFileSync(script, entrypoint.replaceAll("/app", root));
  const result = spawnSync("sh", [script], { encoding: "utf8", env: { ...process.env, PUID: "", PGID: "" }, timeout: 10000 });
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}\n${result.error ?? ""}`);
  return { root, output: result.stdout };
}

test("entrypoint restores base image when the upgraded executable cannot run", t => {
  // 缺失的解释器稳定复现 exec 失败，不依赖宿主是否安装 QEMU。
  const { root, output } = runEntrypoint(t, "#!/missing-songloft-loader\n");
  assert.match(output, /RUNNING 2\.13\.3 full/);
  assert.equal(readFileSync(join(root, "data/songloft"), "utf8"), binary("2.13.3"));
  assert.match(readFileSync(join(root, "roomtone.backup"), "utf8"), /missing-songloft-loader/);
});

test("entrypoint recovers an executable whose version command fails", t => {
  const { output } = runEntrypoint(t, "#!/bin/sh\necho 'Roomtone Version: 99.0.0'\nexit 1\n");
  assert.match(output, /RUNNING 2\.13\.3 full/);
});

test("entrypoint preserves newer online upgrades in the same channel and build type", t => {
  const { output } = runEntrypoint(t, binary("2.13.4"));
  assert.match(output, /RUNNING 2\.13\.4 full/);
});

test("entrypoint initializes missing data binary and respects channel and build switches", async t => {
  for (const [name, target] of [["missing", null], ["channel", binary("dev")], ["build type", binary("2.13.4", "lite")]]) {
    await t.test(name, t => {
      const { output } = runEntrypoint(t, target);
      assert.match(output, /RUNNING 2\.13\.3 full/);
    });
  }
});

test("entrypoint preserves newer dev builds and installs newer base builds", async t => {
  for (const [name, targetTime, sourceTime, want] of [
    ["newer data", "2026-10-19_02:00:00", "2026-10-19_01:00:00", "02"],
    ["newer base", "2026-10-19_01:00:00", "2026-10-19_02:00:00", "02"],
  ]) {
    await t.test(name, t => {
      const { root } = runEntrypoint(t, binary("dev", "lite", targetTime), binary("dev", "lite", sourceTime));
      const version = execFileSync(join(root, "data/songloft"), ["-version"], { encoding: "utf8" });
      assert.match(version, new RegExp(`Build Time: 2026-10-19_${want}:00:00`));
    });
  }
});
