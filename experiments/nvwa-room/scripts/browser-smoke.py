#!/usr/bin/env python3
"""Own isolated headless browser processes; never attaches to an existing browser."""
import argparse,json,pathlib,subprocess,time,urllib.request,os,signal,shutil
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);p.add_argument('--browser',default='/opt/homebrew/bin/chromium');a=p.parse_args();s=pathlib.Path(a.scratch).resolve();out=s/'browser-smoke';out.mkdir(exist_ok=True)
children=[];files=[]
def start(args,log):
 f=open(out/log,'w');files.append(f);child=subprocess.Popen(args,stdout=f,stderr=f);children.append(child);return child
try:
 topology=start([str(s/'bin/topology'),'-bin',str(s/'bin'),'-out',str(out/'topology')],'topology.log')
 deadline=time.monotonic()+20
 while not (out/'topology/topology.json').exists():
  if topology.poll() is not None:raise RuntimeError('topology failed; see log')
  if time.monotonic()>deadline:raise RuntimeError('topology timeout')
  time.sleep(.1)
 config=json.loads((out/'topology/topology.json').read_text())
 for index,hz in enumerate([440,880]):
  wav=out/f'tone-{hz}.wav'
  subprocess.run(['ffmpeg','-v','error','-f','lavfi','-i',f'sine=frequency={hz}:sample_rate=48000:duration=45','-ac','1','-y',str(wav)],check=True)
  args=[a.browser,'--headless=new','--no-first-run','--no-default-browser-check','--disable-gpu','--no-proxy-server','--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream',f'--use-file-for-fake-audio-capture={wav}','--autoplay-policy=no-user-gesture-required',f'--user-data-dir={out / ("profile-"+str(hz))}',config['room_url']+f'/web/?auto=1&count=90&tone={hz}']
  start(args,f'chromium-{hz}.log');time.sleep(.5)
 deadline=time.monotonic()+30;paths=[]
 roomlog=out/'topology/room.log'
 while time.monotonic()<deadline:
  if roomlog.exists():paths=[line.split('browser_evidence=',1)[1].strip() for line in roomlog.read_text().splitlines() if 'browser_evidence=' in line]
  if len(paths)>=2:break
  time.sleep(.2)
 if len(paths)!=2:raise RuntimeError(f'expected two browser receipts; got {len(paths)}')
 failed=False
 for i,path in enumerate(paths):
  data=json.loads(pathlib.Path(path).read_text());(out/f'player-{i+1}.json').write_text(json.dumps(data,indent=2));summary={k:v for k,v in data.items() if k not in ['messages','journal','audio_samples','final_snapshot']};print(json.dumps(summary),flush=True)
  if data.get('error') or data.get('peer_tone_samples',0)<10 or data.get('peer_over_self_db',0)<15:failed=True
 if failed:raise RuntimeError('browser peer tone assertion failed')
finally:
 for child in reversed(children):
  if child.poll() is None:
   child.terminate()
   try:child.wait(timeout=6)
   except subprocess.TimeoutExpired:child.kill();child.wait(timeout=3)
 for f in files:f.close()
