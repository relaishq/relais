#!/usr/bin/env python3
"""Validate and export six review-fix WS runs without replacing stage-2 history."""
import argparse,gzip,hashlib,json,pathlib
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);p.add_argument('--out',required=True);a=p.parse_args()
s=pathlib.Path(a.scratch).resolve();out=pathlib.Path(a.out);out.mkdir(parents=True,exist_ok=True);raw=out/'raw';raw.mkdir(exist_ok=True)
rows=[];manifest=[]
for fault in ['disconnect','worker-kill']:
 for trial in range(1,4):
  name=f'{fault}-{trial}';path=s/'ws-matrix'/name/'result.json';data=json.loads(path.read_text());proof=data['in_flight_cut'];metrics=data['room_metrics'];players=data['players']
  assert not data['errors'],(name,data['errors'])
  topology=json.loads((path.parent/'topology.json').read_text())
  assert int(topology['redis'].rsplit(':',1)[1]) not in [6379,9101,9201]
  worker=next(child for child in topology['children'] if child['name']=='worker-1')
  if fault=='worker-kill':assert data['kill_pid']==worker['pid']
  assert data['transport']=='websocket' and data['messages_per_client']==1000 and data['rate_hz']==30
  assert proof['pending_at_cut'] and proof['original_error']=='connection_closed'
  assert proof['sent_ns']<=proof['applied_barrier_ns']<=proof['cut_ns']<proof['sent_ns']+2_000_000_000
  assert proof['old_generation']==1 and proof['new_generation']==2
  assert proof['query']==proof['retry'] and proof['query']['applied']
  assert proof['deduplicated']==metrics['deduplicated']==1
  assert proof['side_effects']==metrics['held_command_applications']==1 and metrics['applied']==1000
  assert metrics['held_command_id']==proof['command_id']
  assert [v['reconnects'] for v in players]==[1,0]
  for player in players:
   assert len(player['messages'])==1000 and player['missing']==0 and not player['errors']
   if player is players[0]:
    assert not any(o['at_ns']<=proof['cut_ns'] and (o['reply'].get('result') or {}).get('command_id')==proof['command_id'] for o in player['observations'])
   for audio in player['audio_sessions']:
    assert audio['peer_payload']>0 and audio['self_payload']==audio['unknown_payload']==0
  row={'run':name,'command_id':proof['command_id'],'sent_to_cut_ms':(proof['cut_ns']-proof['sent_ns'])/1e6,'generations':[proof['old_generation'],proof['new_generation']],'dedup':proof['deduplicated'],'command_side_effects':proof['side_effects'],'total_applied':metrics['applied'],'rtt_mean_ms':[v['rtt_mean_ms'] for v in players],'rtt_max_ms':[v['rtt_max_ms'] for v in players],'latest_state_age_max_ms':[v['latest_state_age_max_ms'] for v in players],'missing':[v['missing'] for v in players],'duplicate_receipts':[v['duplicate_receipts'] for v in players],'reordered_receipts':[v['reordered_receipts'] for v in players],'reconnects':[v['reconnects'] for v in players],'recovery_ms':players[0]['recovery_ms'],'cut_to_recovered_ms':players[0]['cut_to_recovered_ms'],'audio_max_gap_ms':[v['audio_max_gap_ms'] for v in players],'max_queue_bytes':metrics['max_queue_bytes'],'max_queue_age_ms':metrics['max_queue_age_ms'],'duration_ms':data['duration_ms'],'kill_pid':data.get('kill_pid'),'pending_at_cut':proof['pending_at_cut'],'result_version':proof['query']['room_version']}
  row['owned_redis']=topology['redis'];row['owned_worker_1_pid']=worker['pid']
  rows.append(row)
  target=raw/(name+'.json.gz')
  with target.open('wb') as f:
   with gzip.GzipFile(fileobj=f,mode='wb',mtime=0) as z:z.write(path.read_bytes())
  manifest.append({'run':name,'source':str(path),'file':str(target.relative_to(out)),'sha256':hashlib.sha256(target.read_bytes()).hexdigest()})
(out/'summary.json').write_text(json.dumps(rows,indent=2));(out/'manifest.json').write_text(json.dumps(manifest,indent=2));print(json.dumps(rows,indent=2))
