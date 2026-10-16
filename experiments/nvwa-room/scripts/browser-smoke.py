#!/usr/bin/env python3
"""Own isolated headless browser processes; never attaches to an existing browser."""
import argparse,json,pathlib,subprocess,time,urllib.request,os,signal,shutil,math,struct,wave,tempfile,hashlib
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);p.add_argument('--transport',choices=['ws','dc'],default='ws');p.add_argument('--browser',default='/opt/homebrew/bin/chromium');p.add_argument('--attempt',default='');p.add_argument('--audio-test-mode',choices=['service-unsandboxed','all-unsandboxed'],default='service-unsandboxed');a=p.parse_args();s=pathlib.Path(a.scratch).resolve();out=s/('browser-smoke-'+a.transport+'-'+a.audio_test_mode+('-'+a.attempt if a.attempt else ''));out.mkdir(exist_ok=True)
# Fixtures belong to this execution's working directory, not the caller's scratch.
fixtures=pathlib.Path(tempfile.mkdtemp(prefix='nvwa-browser-fixtures-',dir=pathlib.Path.cwd()))
children=[];files=[]
def start(args,log):
 f=open(out/log,'w');files.append(f);child=subprocess.Popen(args,stdout=f,stderr=f);children.append(child);return child
try:
 topology_args=[str(s/'bin/topology'),'-bin',str(s/'bin'),'-out',str(out/'topology')]
 if a.transport=='dc':topology_args.append('-datachannel')
 topology=start(topology_args,'topology.log')
 deadline=time.monotonic()+20
 while not (out/'topology/topology.json').exists():
  if topology.poll() is not None:raise RuntimeError('topology failed; see log')
  if time.monotonic()>deadline:raise RuntimeError('topology timeout')
  time.sleep(.1)
 config=json.loads((out/'topology/topology.json').read_text())
 assert config['room_url'].startswith('http://127.0.0.1:'), 'sandbox exception is local-test-only'
 for index,hz in enumerate([440,880]):
  wav=fixtures/f'tone-{hz}.wav'
  # Canonical RIFF PCM, signed 16-bit little endian, mono, 48 kHz, no metadata chunks.
  with wave.open(str(wav),'wb') as capture:
   capture.setnchannels(1);capture.setsampwidth(2);capture.setframerate(48000)
   capture.writeframes(b''.join(struct.pack('<h',round(8192*math.sin(2*math.pi*hz*n/48000))) for n in range(48000*2)))
  with wave.open(str(wav),'rb') as capture:
   params=capture.getparams();assert params.nchannels==1 and params.sampwidth==2 and params.framerate==48000 and params.comptype=='NONE'
  metadata={'path':str(wav),'exists':wav.is_file(),'readable':os.access(wav,os.R_OK),'bytes':wav.stat().st_size,'sha256':hashlib.sha256(wav.read_bytes()).hexdigest(),'channels':1,'bits_per_sample':16,'sample_rate':48000,'cwd':str(pathlib.Path.cwd())}
  (out/f'tone-{hz}-fixture.json').write_text(json.dumps(metadata,indent=2));shutil.copy2(wav,out/wav.name)
  print(json.dumps({'fixture':metadata}),flush=True)
  args=[a.browser,'--headless=new','--no-first-run','--no-default-browser-check','--disable-gpu','--no-proxy-server','--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream',f'--use-file-for-fake-audio-capture={wav}','--autoplay-policy=no-user-gesture-required',f'--user-data-dir={out / ("profile-"+str(hz))}',config['room_url']+f'/web/?auto=1&count=90&tone={hz}&transport={a.transport}']
  args.insert(1,'--disable-features=AudioServiceSandbox' if a.audio_test_mode=='service-unsandboxed' else '--no-sandbox')
  (out/f'chromium-{hz}-command.json').write_text(json.dumps(args,indent=2))
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
 shutil.rmtree(fixtures)
