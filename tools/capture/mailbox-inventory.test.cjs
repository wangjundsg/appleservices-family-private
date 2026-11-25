'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const fs=require('node:fs'),os=require('node:os'),path=require('node:path');
const {reconcile,inventory}=require('./mailbox-inventory.cjs');
test('deduplicates addresses, keeps conflicting provenance and never copies credentials',()=>{
  const result=reconcile({legacy:[{address:' Owned@Hotmail.com ',source:'purchased',client_id:'secret-id',refresh_token:'secret-token',password:'secret-password'}],
    workbench:{'owned@hotmail.com':{address:'owned@hotmail.com',source:'owned',reader:'graph',status:'ok'}},
    usage:{'OWNED@hotmail.com':'2026-11-14'},reservations:[{email:'owned@hotmail.com',state:'unknown'}]});
  assert.equal(result.counts.unique,1);assert.equal(result.counts.overlap,1);assert.equal(result.counts.legacyCredentialsSaved,1);
  assert.equal(result.items[0].readOwner,'8802');assert.equal(result.items[0].sourceConflict,true);
  assert.equal(result.items[0].registrationBlocked,true);assert.equal(result.items[0].authorization,'not_checked');
  assert.doesNotMatch(JSON.stringify(result),/secret-|password|refresh_token|client_id/);
});
test('unknown registrations and uncertain protection bindings remain occupied',()=>{
  const result=reconcile({reservations:[{email:'owner@example.test',state:'failed'}],protectionClaims:[
    {address:'active@icloud.com',target_account:'owner@example.test',state:'active'},
    {address:'uncertain@icloud.com',target_account:'owner@example.test',state:'released',binding_uncertain:true},
    {address:'released@icloud.com',target_account:'owner@example.test',state:'released'}]});
  assert.equal(result.items[0].registrationBlocked,true);assert.equal(result.protectionDependencies.length,2);
});
test('filesystem inventory is read-only and reports missing coverage',t=>{
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'mail-inventory-'));t.after(()=>fs.rmSync(dir,{recursive:true,force:true}));
  fs.mkdirSync(path.join(dir,'.cache'),{recursive:true});
  const file=path.join(dir,'.cache/mailboxes.json');const contents='[{"address":"owner@example.test","password":"private"}]';fs.writeFileSync(file,contents);
  const result=inventory({repoRoot:dir,outlookRoot:dir});assert.equal(result.counts.unique,1);
  assert.equal(result.coverage.backupUsagePresent,false);assert.equal(fs.readFileSync(file,'utf8'),contents);
  assert.deepEqual(fs.readdirSync(path.join(dir,'.cache')),['mailboxes.json']);
  fs.writeFileSync(file,'broken');assert.throws(()=>inventory({repoRoot:dir,outlookRoot:dir}),/inventory_file_invalid/);
});
