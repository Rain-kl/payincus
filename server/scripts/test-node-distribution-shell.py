import os,pathlib,tempfile,subprocess
with tempfile.TemporaryDirectory(prefix='node-installer-') as directory:
 repo=pathlib.Path(__file__).resolve().parents[2]
 root=pathlib.Path(directory);binary=root/'curl'
 binary.write_text('''#!/usr/bin/python3
import os,sys,pathlib
args=sys.argv[1:]
url=next(a for a in args if a.startswith('https://'))
expected='https://raw.githubusercontent.com/Rain-kl/incus/'+os.environ['EXPECTED_BRANCH']+'/scripts/payincus/install.sh'
assert url==expected
assert '--proto' in args and args[args.index('--proto')+1]=='=https'
if os.environ.get('FAIL_DOWNLOAD')=='1':sys.exit(22)
path=pathlib.Path(args[args.index('-o')+1])
path.write_text('''+repr('''#!/bin/bash
set -eu
[[ "$1" == "$EXPECTED_CHANNEL" ]]
[[ "${INCUDAL_PANEL_URL:-}" == 'https://panel.example.com' ]]
[[ "${INCUDAL_AGENT_INSTALL_TOKEN:-}" == 'ait_fixture_not_a_real_secret' ]]
printf 'NATIVE_INSTALLER_REACHED\\n'
''')+''')
''');binary.chmod(0o755)
 for name in ['agent-install.sh','install.sh']:
  for channel,branch in [('latest','main'),('canary','canary')]:
   env={**os.environ,'PATH':str(root)+':'+os.environ['PATH'],'INCUS_NODE_CHANNEL':channel,'EXPECTED_BRANCH':branch,'EXPECTED_CHANNEL':channel,'INCUDAL_PANEL_URL':'https://panel.example.com','INCUDAL_AGENT_INSTALL_TOKEN':'ait_fixture_not_a_real_secret'}
   script=str(repo/'server/templates'/name)
   result=subprocess.run(['bash',script],env=env,capture_output=True,text=True)
   assert result.returncode==0 and 'NATIVE_INSTALLER_REACHED' in result.stdout,(name,channel,'handoff failed')
   failed=subprocess.run(['bash',script],env={**env,'FAIL_DOWNLOAD':'1'},capture_output=True,text=True)
   assert failed.returncode!=0 and 'NATIVE_INSTALLER_REACHED' not in failed.stdout
   invalid=subprocess.run(['bash',script,'--uninstall'],env=env,capture_output=True,text=True)
   assert invalid.returncode!=0 and 'NATIVE_INSTALLER_REACHED' not in invalid.stdout
 print('Shell integration: both entrypoints/channels delegate to Incus with masked TUI handoff; download failure and unsupported flags fail closed')
