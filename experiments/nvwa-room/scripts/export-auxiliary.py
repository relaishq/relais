#!/usr/bin/env python3
"""Preserve stage-3 auxiliary outcomes without copying profiles or credentials."""
import argparse,gzip,hashlib,json,pathlib
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);p.add_argument('--dest',required=True);a=p.parse_args();s=pathlib.Path(a.scratch).resolve();dest=pathlib.Path(a.dest);out=dest/'auxiliary';out.mkdir(parents=True,exist_ok=True);manifest={};summary={}
def preserve(source,target):
 target=out/target;target.parent.mkdir(parents=True,exist_ok=True)
 payload=source.read_bytes();target.write_bytes(gzip.compress(payload,mtime=0));manifest[str(target.relative_to(dest))]={'sha256':hashlib.sha256(target.read_bytes()).hexdigest(),'source':str(source),'uncompressed_bytes':len(payload)}
for transport in ['dc','ws']:
 source=s/f'burst-{transport}-recheck/result.json'
 if not source.exists():source=s/f'burst-{transport}/result.json'
 data=json.loads(source.read_text())
 assert data['messages_per_client']==200 and data['payload_bytes']==4
 row=[]
 for player in data['players']:
  receipts=player['receipts'];assert [r['seq'] for r in receipts]==list(range(200))
  audio=player['audio'];assert audio['peer_payload']>=player['peer_audio_before']+20 and audio['self_payload']==audio['unknown_payload']==0
  assert not player['error'] and player['snapshot']['room_version']==2
  row.append({'sent':200,'received':len(receipts),'send_duration_ms':player['send_duration_ms'],'rtt_mean_ms':sum(r['rtt_ms'] for r in receipts)/200,'rtt_max_ms':max(r['rtt_ms'] for r in receipts),'peer_audio_before':player['peer_audio_before'],'peer_audio_after':audio['peer_payload'],'audio_gap_ms':audio['max_gap_ms']})
 summary['burst-'+transport]={'players':row,'metrics':data['room_metrics'],'lane':data['lane']};preserve(source,f'burst-{transport}.json.gz')
 data=json.loads((s/f'backpressure-{transport}/result.json').read_text());assert data['applied']==data['metrics']['applied']==80 and data['metrics']['slow_consumers']==1 and data['metrics']['max_queue_bytes']<=16384
 if not data['typed_slow_consumer']:assert transport=='dc' and data['client_close_error']
 summary['backpressure-'+transport]={k:v for k,v in data.items() if k!='observations'};preserve(s/f'backpressure-{transport}/result.json',f'backpressure-{transport}.json.gz')
for name in ['burst-dc','probe-burst-dc']:
 source=s/name/'result.json'
 if source.exists():preserve(source,'initial-'+name+'.json.gz')
policy=json.loads((s/'policies-dc/result.json').read_text());assert policy['move_http_status']==409 and policy['observed_upstream_reconnect_required'] and policy['new_generation']==2
summary['policies']={k:v for k,v in policy.items() if k not in ['old_observations','new_observations','snapshot','retry','command_result','audio','move_snapshot']};preserve(s/'policies-dc/result.json','policies-dc.json.gz')
for transport in ['ws','dc']:
 directory=s/f'browser-smoke-{transport}-service-unsandboxed-media-sink';players=[]
 for file in sorted(directory.glob('player-*.json')):
  data=json.loads(file.read_text());assert not data['error'] and data['count']==90 and len(data['messages'])==90
  assert data['peer_tone_samples']>=10 and data['peer_over_self_db']>=15
  assert all(not m.get('error') for m in data['messages'])
  players.append({k:data[k] for k in ['player_id','own_hz','peer_hz','peer_tone_samples','peer_over_self_db','count']})
 assert len(players)==2 and {p['own_hz'] for p in players}=={440,880};summary['browser-'+transport]=players
# Preserve failed exploratory browser attempts alongside successful final ones.
for directory in sorted(s.glob('browser-smoke-*')):
 for pattern in ['player-*.json','chromium-*.log','chromium-*-command.json','tone-*-fixture.json']:
  for file in directory.glob(pattern):preserve(file,pathlib.Path(directory.name)/(file.name+'.gz'))
for gate in ['unit','harness']:
 result=json.loads((s/gate/'result.json').read_text());summary[gate]=result
 for file in [s/gate/'result.json',s/gate/('test-unit.log' if gate=='unit' else 'test-harness-short.log')]:preserve(file,pathlib.Path(gate)/(file.name+'.gz'))
for name in ['lint-final.log','vet.log','stock.log','experiments-race-10.log','experiments-race-10-initial-failure.log','experiments-race-10-final.log','verification.json','wefty-runs.json','stage3-run.json','burst-rechecks.json','burst-recheck-sha256.txt','burst-recheck-runtime-source-sha256.json']:
 if (s/name).exists():preserve(s/name,name+'.gz')
(dest/'auxiliary-summary.json').write_text(json.dumps(summary,indent=2));(dest/'auxiliary-manifest.json').write_text(json.dumps(manifest,indent=2));print('Validated burst, policies, pressure and browser receipts; exported',len(manifest),'artifacts')
