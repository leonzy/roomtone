#!/usr/bin/env python3
"""Rebind Roomtone's library after a late NAS disk mount, without starting audio."""
import argparse
import json
import os
from pathlib import Path
import subprocess


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


def check(config, dry_run=False):
    marker = Path(config["state_file"])
    active, ready = running(config["container"]), disk_ready(config)
    matched = ready and active and same_mount(config)
    action = "none"
    if not ready and active:
        action = "wait-for-disk"
    elif ready and ((active and not matched) or marker.exists()):
        action = "rebind"
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
    command(["/usr/bin/docker", "compose", "-p", config["compose_project"], "-f", config["compose_file"],
             "up", "-d", "--no-deps", "--pull", "never", "--force-recreate", config["compose_service"]])
    command([appctl, "start", config["app_id"]])
    if not same_mount(config):
        command([appctl, "stop", config["app_id"]])
        raise RuntimeError("Library binding still differs after recreation; application left stopped")
    marker.unlink(missing_ok=True)
    print("Roomtone library binding restored; original application settings retained.", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("--dry-run", action="store_true")
    options = parser.parse_args()
    check(json.loads(Path(options.config).read_text()), options.dry_run)
