'use strict';
((root) => {
  const object=(x)=>x!==null && typeof x==='object' && !Array.isArray(x);
  const integer=(x,max=Number.MAX_SAFE_INTEGER)=>Number.isSafeInteger(x) && x>=0 && x<=max;
  const age=(x)=>Number.isFinite(x) && x>=0;
  function samples(p) {
    if(!object(p) || !integer(p.count) || p.count===0 || !integer(p.phase_samples,23999))return null;
    const total=(p.count-1)*24000+p.phase_samples;
    return integer(total)?total:null;
  }
  const validProgress=(p)=>object(p) && integer(p.SSRC,0xffffffff) && integer(p.Index) && integer(p.Timestamp,0xffffffff) && integer(p.Consumed);
  const samePosition=(a,b)=>samples(a)===samples(b);
  const sameProgress=(a,b)=>['SSRC','Index','Timestamp','Consumed'].every((key)=>a[key]===b[key]);
  const validWindows=(windows)=>windows===null || Array.isArray(windows) && windows.every((w)=>object(w) && integer(w.SSRC,0xffffffff) && integer(w.First) && integer(w.Last) && w.First<=w.Last);
  function verdict(kind,evidence) {
    const result=(status,reasons,extra={})=>({status,reasons,...extra});
    if(!object(evidence) || !Array.isArray(evidence.errors) || !evidence.errors.every((x)=>typeof x==='string'))return result('inconclusive',['agent evidence or errors missing or invalid']);
    if(evidence.errors.length)return result('inconclusive',evidence.errors);
    const before=evidence.before,after=evidence.after,observed=evidence.observed_after,r=after?.last_resume,checkpoint=after?.checkpoint;
    // First-event status legitimately has no last_resume. Once present, its
    // number must be valid; do not silently coerce absent nested evidence to 0.
    const previous=before?.last_resume;
    if(!['move','drain','kill'].includes(kind) || samples(before?.position)===null || samples(after?.position)===null || samples(observed?.position)===null ||
       !validProgress(after?.progress) || !validProgress(observed?.progress) || !object(r) || !object(checkpoint) ||
       samples(r.position)===null || samples(checkpoint.position)===null || !validProgress(r.progress) || !validProgress(checkpoint.progress) ||
       !['planned_move','takeover'].includes(r.kind) || !integer(r.number) || r.number===0 ||
       (previous!==undefined && (!object(previous) || !integer(previous.number))) ||
       !object(observed.last_resume) || !integer(observed.last_resume.number) || observed.last_resume.number===0 ||
       typeof r.input_may_be_duplicated!=='boolean' || !validWindows(r.duplicate_windows) || !age(r.checkpoint_age_ms) || !age(r.snapshot_age_ms))return result('inconclusive',['agent position, progress, resume, checkpoint or observation missing or invalid']);
    if(kind==='kill' && (!age(evidence.measured_checkpoint_age_ms) || !age(evidence.measured_snapshot_age_ms)))return result('inconclusive',['measured checkpoint ages missing or invalid']);
    if(kind!=='kill' && (samples(evidence.exported?.position)===null || !validProgress(evidence.exported?.progress)))return result('inconclusive',['flushed source export missing or invalid']);
    const extras={positionBefore:before.position,positionRestored:r.position,positionAfter:observed.position,inputMayBeDuplicated:r.input_may_be_duplicated,duplicateWindows:r.duplicate_windows,checkpointAgeMs:r.checkpoint_age_ms,snapshotAgeMs:r.snapshot_age_ms};
    if(r.kind!==(kind==='kill'?'takeover':'planned_move') || r.number!==(previous?.number ?? 0)+1 || observed.last_resume.number!==r.number)return result('fail',['resume does not belong to this event'],extras);
    if(!samePosition(r.position,checkpoint.position) || !sameProgress(r.progress,checkpoint.progress))return result('fail',['restore differs from the actual incoming snapshot'],extras);
    // Index and Timestamp may repeat inside a duplicate window (and RTP time
    // wraps). Consumed counts callbacks; count/phase counts only fresh audio.
    // Both must advance after the immediate observation, independently of replay.
    if(samples(after.position)<samples(r.position) || after.progress.Consumed<r.progress.Consumed ||
       samples(observed.position)<=samples(after.position) || observed.progress.Consumed<=after.progress.Consumed)return result('fail',['agent did not advance after the immediate post-resume observation'],extras);
    if(r.duplicate_windows?.length && !r.input_may_be_duplicated)return result('fail',['duplicate input windows contradict the resume flag'],extras);
    if(kind==='kill') {
      const checkpointAge=evidence.measured_checkpoint_age_ms,snapshotAge=evidence.measured_snapshot_age_ms;
      if(Math.abs(checkpointAge-r.checkpoint_age_ms)>0.001 || Math.abs(snapshotAge-r.snapshot_age_ms)>0.001 || snapshotAge<checkpointAge)return result('fail',['resume age differs from the control-plane measurement'],extras);
      const rollbackMs=Math.max(0,(samples(before.position)-samples(r.position))/48);
      extras.rollbackMs=rollbackMs;
      // Snapshot age includes copy-to-commit time; write age alone can hide
      // storage latency. One 20 ms packet covers count quantization at capture.
      if(rollbackMs>snapshotAge+20)return result('fail',['rollback exceeds measured snapshot age plus one audio packet'],extras);
    } else {
      if(!samePosition(evidence.exported.position,r.position) || !sameProgress(evidence.exported.progress,r.progress))return result('fail',['planned move changed count, tone phase or consumed progress'],extras);
      if(r.input_may_be_duplicated)return result('fail',['planned move unexpectedly flagged duplicated input'],extras);
    }
    return result('pass',[],extras);
  }
  root.relaisContinuity={verdict,samples};
  if(typeof module==='object')module.exports=root.relaisContinuity;
})(typeof window==='object'?window:globalThis);
