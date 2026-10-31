import importlib.util
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
        self.assertEqual(commands[-1], ["/usr/local/bin/appcenter-cli", "start", "Songloft"])
        self.assertFalse(Path(self.config["state_file"]).exists())

    def test_only_guard_paused_app_resumes_when_disk_returns(self):
        Path(self.config["state_file"]).write_text("waiting")
        commands = self.run_guard(False, True, [True])
        self.assertIn("--force-recreate", commands[0])
        self.assertFalse(Path(self.config["state_file"]).exists())

    def test_failed_repair_keeps_application_stopped(self):
        with self.assertRaises(RuntimeError):
            self.run_guard(True, True, [False, False])
        self.assertTrue(Path(self.config["state_file"]).exists())


if __name__ == "__main__":
    unittest.main()
