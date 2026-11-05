#!/usr/bin/env python3
"""Rebind Roomtone's library after a late NAS disk mount, without starting audio."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time


def command(args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=120).stdout.strip()


def running(container):
    try:
        return command(["/usr/bin/docker", "inspect", "--format", "{{.State.Running}}", container]) == "true"
    except subprocess.CalledProcessError:
        return False


def disk_ready(config):
    disk, library = Path(config["disk_root"]), Path(config["library_path"])
    return os.path.ismount(disk) and library.is_dir() and disk.resolve() in library.resolve().parents


def same_mount(config):
    try:
        host = os.stat(config["library_path"])
        value = command(["/usr/bin/docker", "exec", config["container"], "stat", "-c", "%d:%i", config["container_path"]])
        return value == f"{host.st_dev}:{host.st_ino}"
    except (OSError, subprocess.CalledProcessError):
        return False


def application_state(config):
    try:
        return command(["/usr/local/bin/appcenter-cli", "status", config["app_id"]])
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        return ""


def boot_id():
    path = Path("/proc/sys/kernel/random/boot_id")
    return path.read_text().strip() if path.exists() else "local"


def entry_repair_due(config, dry_run):
    """Retry a failed requested start, never restart an explicitly stopped app."""
    pending = Path(config["state_file"] + ".entry-pending")
    if application_state(config) != "start":
        if not dry_run:
            pending.unlink(missing_ok=True)
        return False
    now, boot = time.time(), boot_id()
    try:
        observed = json.loads(pending.read_text())
    except (OSError, ValueError):
        observed = {}
    if observed.get("boot") != boot or not isinstance(observed.get("since"), (int, float)) or now < observed["since"]:
        if not dry_run:
            pending.write_text(json.dumps({"boot": boot, "since": now}))
        return False
    # Leave normal app-center startup time to finish before repairing registration.
    return now - observed["since"] >= 90


def check(config, dry_run=False):
    marker = Path(config["state_file"])
    active, ready = running(config["container"]), disk_ready(config)
    matched = ready and active and same_mount(config)
    action = "none"
    if not ready and active:
        action = "wait-for-disk"
    elif ready and ((active and not matched) or marker.exists()):
        action = "rebind"
    elif matched and entry_repair_due(config, dry_run):
        action = "repair-entry"
    if dry_run:
        print(json.dumps({"disk_ready": ready, "container_running": active, "same_mount": matched, "guard_paused": marker.exists(), "action": action}))
        return
    if action == "none":
        return
    # A user-stopped application is left alone. Only guard-paused apps resume.
    marker.write_text("Waiting for the configured music disk\n")
    appctl = "/usr/local/bin/appcenter-cli"
    if active:
        command([appctl, "stop", config["app_id"]])
    if action == "wait-for-disk":
        print("Music disk unavailable; Roomtone paused to protect its library records.", flush=True)
        return
    # Recheck after stopping: the disk can disappear during application shutdown.
    if not disk_ready(config):
        return
    if action == "rebind":
        # Leave the container stopped. Starting it here makes fnOS take its
        # "already running" shortcut without registering the desktop entry.
        command(["/usr/bin/docker", "compose", "-p", config["compose_project"], "-f", config["compose_file"],
                 "create", "--no-build", "--pull", "never", "--force-recreate", config["compose_service"]])
    command([appctl, "start", config["app_id"]])
    if not same_mount(config):
        command([appctl, "stop", config["app_id"]])
        raise RuntimeError("Library binding still differs after recreation; application left stopped")
    if application_state(config) != "running":
        raise RuntimeError("Roomtone container started but fnOS application registration did not finish")
    marker.unlink(missing_ok=True)
    Path(config["state_file"] + ".entry-pending").unlink(missing_ok=True)
    print("Roomtone library binding restored; original application settings retained.", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("--dry-run", action="store_true")
    options = parser.parse_args()
    check(json.loads(Path(options.config).read_text()), options.dry_run)
