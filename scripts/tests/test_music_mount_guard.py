import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("music_mount_guard", Path(__file__).parents[1] / "leonfn" / "music_mount_guard.py")
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class MusicMountGuardTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.config = dict(container="roomtone", disk_root="/disk", library_path="/disk/music", container_path="/app/music",
                           state_file=str(Path(self.tmp.name) / "paused"), app_id="Songloft", compose_project="songloft",
                           compose_file="/app/docker-compose.yaml", compose_service="songloft")

    def run_guard(self, active, ready, matched):
        with patch.object(guard, "running", return_value=active), patch.object(guard, "disk_ready", return_value=ready), \
             patch.object(guard, "application_state", return_value="running"), \
             patch.object(guard, "same_mount", side_effect=matched), patch.object(guard, "command") as cmd:
            guard.check(self.config)
            return [call.args[0] for call in cmd.call_args_list]

    def test_user_stopped_app_is_not_restarted(self):
        self.assertEqual(self.run_guard(False, True, []), [])

    def test_correct_binding_is_not_restarted(self):
        self.assertEqual(self.run_guard(True, True, [True]), [])

    def test_missing_disk_stops_app_without_recreating_empty_library(self):
        commands = self.run_guard(True, False, [])
        self.assertEqual(commands, [["/usr/local/bin/appcenter-cli", "stop", "Songloft"]])
        self.assertTrue(Path(self.config["state_file"]).exists())

    def test_late_mount_repairs_stale_binding_and_keeps_fn_entry(self):
        commands = self.run_guard(True, True, [False, True])
        self.assertIn("--force-recreate", commands[1])
        self.assertIn("create", commands[1])
        self.assertNotIn("up", commands[1])
        self.assertEqual(commands[-1], ["/usr/local/bin/appcenter-cli", "start", "Songloft"])
        self.assertFalse(Path(self.config["state_file"]).exists())

    def test_only_guard_paused_app_resumes_when_disk_returns(self):
        Path(self.config["state_file"]).write_text("waiting")
        commands = self.run_guard(False, True, [True])
        self.assertIn("--force-recreate", commands[0])
        self.assertIn("create", commands[0])
        self.assertFalse(Path(self.config["state_file"]).exists())

    def test_failed_repair_keeps_application_stopped(self):
        with self.assertRaises(RuntimeError):
            self.run_guard(True, True, [False, False])
        self.assertTrue(Path(self.config["state_file"]).exists())

    def test_stalled_requested_start_repairs_entry_after_grace_without_compose_up(self):
        pending = Path(self.config["state_file"] + ".entry-pending")
        with patch.object(guard, "running", return_value=True), patch.object(guard, "disk_ready", return_value=True), \
             patch.object(guard, "same_mount", return_value=True), patch.object(guard, "boot_id", return_value="boot-a"), \
             patch.object(guard, "application_state", return_value="start") as state, \
             patch.object(guard.time, "time", return_value=100) as clock, patch.object(guard, "command") as cmd:
            guard.check(self.config)
            self.assertTrue(pending.exists())
            cmd.assert_not_called()
            clock.return_value = 189
            guard.check(self.config)
            cmd.assert_not_called()
            clock.return_value = 191
            state.side_effect = ["start", "running"]
            guard.check(self.config)
            self.assertEqual([c.args[0] for c in cmd.call_args_list], [
                ["/usr/local/bin/appcenter-cli", "stop", "Songloft"],
                ["/usr/local/bin/appcenter-cli", "start", "Songloft"],
            ])
            self.assertFalse(pending.exists())

    def test_stopped_application_does_not_get_entry_repair(self):
        pending = Path(self.config["state_file"] + ".entry-pending")
        pending.write_text(json.dumps({"boot": "boot-a", "since": 0}))
        with patch.object(guard, "running", return_value=True), patch.object(guard, "disk_ready", return_value=True), \
             patch.object(guard, "same_mount", return_value=True), patch.object(guard, "application_state", return_value="stop"), \
             patch.object(guard, "command") as cmd:
            guard.check(self.config)
            cmd.assert_not_called()
            self.assertFalse(pending.exists())

    def test_reboot_resets_grace_and_dry_run_does_not_write_markers(self):
        pending = Path(self.config["state_file"] + ".entry-pending")
        with patch.object(guard, "application_state", return_value="start"), \
             patch.object(guard, "boot_id", return_value="new-boot"), patch.object(guard.time, "time", return_value=100):
            self.assertFalse(guard.entry_repair_due(self.config, True))
            self.assertFalse(pending.exists())
            pending.write_text(json.dumps({"boot": "old-boot", "since": 0}))
            self.assertFalse(guard.entry_repair_due(self.config, False))
            self.assertEqual(json.loads(pending.read_text())["boot"], "new-boot")


if __name__ == "__main__":
    unittest.main()
