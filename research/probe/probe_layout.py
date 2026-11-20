import subprocess,os,sys
S=os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ADID='system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid'
def run(req):
    r=subprocess.run([S+'/ipswrun-probe','--stream-compressed','--cache',S+'/matrix-cache/22A380','--timeout','600','--daemon','com.apple.adid='+ADID,'22A380',S+'/adiq',req],capture_output=True,text=True)
    out=r.stdout.strip().replace('answer 1300, ','')
    return out if out else 'CRASH'
ops=['0b2b3196','385b2ce8','cfe0b46a','85fe63b0','72096fe6','7715488c','b0eda7af','a54951ec','632b8d6e','55a637c0','716bd86c','9679d4b5']
dsid='fffffffffffffffe'
variants={'none':'','third0+dsid':None,'dsid':dsid,'dsid+0':dsid+'00000000','dsid+0+0':dsid+'0000000000000000','dsid+len4+4':dsid+'0000000411223344','dsid+len4+4+len4+4':dsid+'00000004112233440000000455667788'}
for op in ops:
    row=[]
    for name,p in variants.items():
        req='00000001'+op+('00000000'+dsid if p is None else '00000001'+p)
        row.append('%s=%s'%(name,run(req)))
    print(op,' | '.join(row),flush=True)
