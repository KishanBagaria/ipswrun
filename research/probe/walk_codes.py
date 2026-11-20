import subprocess,re,os,sys
S=os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ADID='system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid'
K=0x3949f873
def run(op):
    env=dict(os.environ,PROBECMP='adid',PROBEMAGIC=hex(op))
    req='00000001%08x00000001fffffffffffffffe'%op
    r=subprocess.run([S+'/ipswrun-probe','--stream-compressed','--cache',S+'/matrix-cache/22A380','--timeout','600','--daemon','com.apple.adid='+ADID,'22A380',S+'/adiq',req],env=env,capture_output=True,text=True)
    consts=set()
    t=(op+K)&0xFFFFFFFF
    for m in re.finditer(r'PROBE CMP rn=(0x[0-9a-f]+) rm=(0x[0-9a-f]+)',r.stderr):
        a,b=int(m.group(1),16),int(m.group(2),16)
        if a==t: consts.add(b)
        elif b==t: consts.add(a)
    return r.stdout.strip(),consts
seen={}; queue=[0xdeadbeef]; allc=set()
while queue and len(seen)<60:
    op=queue.pop(0)
    if op in seen: continue
    out,consts=run(op)
    seen[op]=out
    print('%08x -> %s ; cmp %s'%(op,out,' '.join('%08x'%((c-K)&0xFFFFFFFF) for c in sorted(consts))),flush=True)
    for c in consts:
        allc.add(c)
        for d in (0,):
            cand=(c-K+d)&0xFFFFFFFF
            if cand not in seen and cand not in queue: queue.append(cand)
