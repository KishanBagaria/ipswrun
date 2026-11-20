import subprocess,os
S=os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ADID='system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid'
def run(req):
    r=subprocess.run([S+'/ipswrun-probe','--stream-compressed','--cache',S+'/matrix-cache/22A380','--timeout','600','--daemon','com.apple.adid='+ADID,'22A380',S+'/adiq',req],capture_output=True,text=True)
    out=r.stdout.strip().replace('answer 1300, ','')
    return out if out else 'CRASH'
d='fffffffffffffffe'
L=lambda n:'%08x'%n
B=lambda n:'ab'*n
tests=[
 ('716bd86c dsid,len=16,16 bytes', '716bd86c', d+L(16)+B(16)),
 ('716bd86c dsid,len=16,8 bytes',  '716bd86c', d+L(16)+B(8)),
 ('716bd86c dsid,len=200,200',     '716bd86c', d+L(200)+B(200)),
 ('716bd86c len=16,16 (no dsid)',  '716bd86c', L(16)+B(16)),
 ('a54951ec dsid,len=16,16',       'a54951ec', d+L(16)+B(16)),
 ('a54951ec dsid,len=16,8',        'a54951ec', d+L(16)+B(8)),
 ('632b8d6e sess=1,len=8,8,len=8,8','632b8d6e', L(1)+L(8)+B(8)+L(8)+B(8)),
 ('632b8d6e sess=1,len=8,4',       '632b8d6e', L(1)+L(8)+B(4)),
 ('632b8d6e dsid,sess,len..',      '632b8d6e', d+L(1)+L(8)+B(8)+L(8)+B(8)),
 ('55a637c0 sess=1',               '55a637c0', L(1)),
 ('55a637c0 2 bytes',              '55a637c0', 'abcd'),
 ('0b2b3196 dsid,len=16,16',       '0b2b3196', d+L(16)+B(16)),
 ('0b2b3196 dsid,len=16,16,len=16,16','0b2b3196', d+L(16)+B(16)+L(16)+B(16)),
 ('0b2b3196 dsid,u64,len16,16',    '0b2b3196', d+'0000000000000001'+L(16)+B(16)),
 ('85fe63b0 dsid,u64',             '85fe63b0', d+'0000000000000011'),
 ('b0eda7af dsid,u64',             'b0eda7af', d+'0000000000000011'),
 ('7715488c dsid',                 '7715488c', d),
]
for name,op,p in tests:
    print('%-36s %s'%(name,run('00000001'+op+'00000001'+p)),flush=True)
