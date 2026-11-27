"use strict";
const {test}=require('node:test'),assert=require('node:assert/strict');
const {verdictFrom}=require('./export-captcha-dataset.cjs');
const fs=require('node:fs'),os=require('node:os'),path=require('node:path'),{createHash}=require('node:crypto');
const {record}=require('./captcha-samples.cjs');
const {execFileSync}=require('node:child_process');
test('legacy diagnostics never certify a CAPTCHA label in either language',()=>{
  for(const text of ['', '输入的字符不正确', '验证你的电子邮件地址', 'wrong captcha', 'Verify your email', 'Welcome'])
    assert.equal(verdictFrom(text),'unknown');
});

test('source-paired samples survive later image replacement, remain unverified, and never abort on I/O failure',()=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'captcha-sample-test-'));
  try {
    const png=path.join(root,'captcha.png'),bytes=Buffer.from('fixture-image'),digest=createHash('sha256').update(bytes).digest('hex');
    fs.writeFileSync(png,bytes);
    assert.equal(record(root,{png,digest},'aB3D'),true);
    fs.writeFileSync(png,'replacement-image');
    assert.equal(record(root,{png,digest},'wrong'),false);
    fs.writeFileSync(png,bytes);
    assert.equal(record(path.join(root,'missing'),{png,digest},'aB3D'),false);
    const out=path.join(root,'export');
    execFileSync(process.execPath,[path.join(__dirname,'export-captcha-dataset.cjs'),'--root',root,'--out',out]);
    const csv=fs.readFileSync(path.join(out,'labels.csv'),'utf8');
    assert.match(csv,/,aB3D,unknown,/);assert.match(csv,/,false,source_before_submit/);
    const image=fs.readdirSync(path.join(out,'images'))[0];
    assert.deepEqual(fs.readFileSync(path.join(out,'images',image)),bytes);
  } finally {fs.rmSync(root,{recursive:true,force:true});}
});
