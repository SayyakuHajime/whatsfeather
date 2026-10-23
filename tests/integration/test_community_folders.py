"""Native Community Folders UI projection; all profiles/content are synthetic."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[2]

class CommunityFoldersDesktop(unittest.TestCase):
    def run_native(self,theme,live=False,cache=False):
        with tempfile.TemporaryDirectory(prefix='wf-cf-') as tmp:
            base=Path(tmp)
            for name in ('config','data','cache','runtime'):(base/name).mkdir(mode=0o700)
            evidence=Path(os.environ['WHATSFEATHER_QA_ROOT']) if os.environ.get('WHATSFEATHER_QA_ROOT') else Path(tempfile.mkdtemp(prefix='wf-community-evidence-'))
            capture=evidence/'community-folders'/theme
            env=dict(os.environ,QT_QPA_PLATFORM='offscreen',QT_QUICK_BACKEND='software',QT_FORCE_STDERR_LOGGING='1',XDG_CONFIG_HOME=str(base/'config'),XDG_DATA_HOME=str(base/'data'),XDG_CACHE_HOME=str(base/'cache'),XDG_RUNTIME_DIR=str(base/'runtime'),WHATSFEATHER_QA_CAPTURE_DIR=str(capture))
            profile=base/'profile'
            args=[str(ROOT/'build/bin/whatsfeather'),'--live' if live else '--demo','--theme',theme,'--data-dir',str(profile),'--community-cache-test' if cache else '--community-test']
            result=subprocess.run(args,env=env,capture_output=True,text=True,timeout=25)
            if live:self.assertFalse(profile.exists(),'Rejected live automation must not initialize a profile')
            return result,capture

    def test_folder_hierarchy_interaction_dark_light_and_compact(self):
        for theme in ('dark','light'):
            with self.subTest(theme=theme):
                run,capture=self.run_native(theme)
                log=run.stdout+run.stderr
                self.assertEqual(run.returncode,0,log)
                self.assertIn('Native community folders PASS',log)
                self.assertNotIn('qrc:',log)
                self.assertNotIn('qml:',log.lower())
                for name in ('collapsed','expanded','search','compact'):
                    self.assertTrue((capture/(name+'.png')).is_file(),name)

    def test_actual_demo_daemon_cache_reaches_native_folder_selection(self):
        for theme in ('dark','light'):
            with self.subTest(theme=theme):
                run,_=self.run_native(theme,cache=True)
                self.assertEqual(run.returncode,0,run.stderr)
                self.assertIn('Community daemon/UI PASS',run.stderr)
                self.assertNotIn('qrc:',run.stderr)

    def test_synthetic_community_probe_refuses_live_mode(self):
        for cache in (False,True):
            with self.subTest(cache=cache):
                run,_=self.run_native('dark',live=True,cache=cache)
                self.assertEqual(run.returncode,2,run.stderr)
                self.assertIn('demo-only',run.stderr)

if __name__=='__main__':unittest.main(verbosity=2)
