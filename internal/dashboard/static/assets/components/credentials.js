import { h } from '../lib/dom.js';
import { get,post,remove,query } from '../lib/api.js';
import { hasRole } from '../lib/state.js';
import { utc } from '../lib/format.js';
import { panel,button,pill,empty,notice,note } from './ui.js';
import { table } from './table.js';
import { formDialog,confirmDelete } from './dialog.js';
export async function credentials(ctx) {
  if(!hasRole('admin')) return notice('Requires admin role','red');
  const data=await get('/api/virtual-keys',{signal:ctx.signal});
  const secret={name:'provider_key',label:'Provider secret',type:'password',required:true,help:'Encrypted in the vault. Never returned after submission.'};
  const create=()=>formDialog('Add Provider credentials',[{name:'name',label:'Credential name',required:true},{name:'provider_name',label:'Configured provider name',required:true},{name:'provider_type',label:'Adapter type',required:true,choices:['openai','anthropic','gemini','azure','bedrock','cohere']},secret,{name:'base_url',label:'Upstream base URL · optional'}],async values=>{await post('/api/virtual-keys',values,{signal:ctx.signal});ctx.refresh();});
  return panel('Provider credentials',h('div',{class:'stack'},notice('Upstream vault credentials, not client API keys. Secrets are masked and never returned.','purple'),data.virtual_keys.length?table('Vault-stored Provider credentials',[{label:'Credential / provider',render:k=>h('div',{},k.name,h('div',{class:'muted'},(k.provider_name||k.name)+' · '+k.provider_type))},{label:'State',render:k=>pill(k.enabled?'Enabled':'Disabled')},{label:'Updated · UTC',render:k=>utc(k.updated_at||k.created_at)},{label:'Actions',render:k=>h('div',{class:'row'},button('Rotate',()=>formDialog('Rotate '+k.name,[secret,{name:'confirm',label:'Type '+k.name+' to confirm',required:true}],async values=>{await post('/api/virtual-keys/rotate',{name:k.name,provider_key:values.provider_key},{signal:ctx.signal});ctx.refresh();},{confirm:k.name,submitLabel:'Rotate'})),button('Revoke',()=>confirmDelete(k.name,async()=>{await remove(query('/api/virtual-keys',{name:k.name}),{signal:ctx.signal});ctx.refresh();},'Requests may fail until a replacement is configured.'),{class:'button danger'}))}],data.virtual_keys):empty('No Provider credentials in the vault','Add credentials for a configured upstream. Client API keys belong in Access & Budgets.',button('Add Provider credentials',create,{class:'button primary'})),note('Environment secrets may not appear here. Applied provider config is in Settings.')),button('Add Provider credentials',create,{icon:'plus'}));
}
