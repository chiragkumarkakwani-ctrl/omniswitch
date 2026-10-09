let session = null;
let providers = null;
export const getSession = () => session;
export function setSession(value) { session = value; if (!value) providers = null; }
export const hasRole = role => !!session && ['viewer', 'member', 'admin', 'owner'].indexOf(session.role) >= ['viewer', 'member', 'admin', 'owner'].indexOf(role);
export const getProviders = () => providers;
export const setProviders = value => { providers = value; };
