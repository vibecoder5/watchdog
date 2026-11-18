const { createApp } = Vue;
createApp({
  data() { return { authenticated: false, password: '', busy: false, error: '', message: '', status: {}, cfg: null, original: '', mountText: '', savedInterval: 15, tab: 'settings', history: { events: [], deliveries: [] }, now: Date.now(), refreshing: false, timer: null, names: { cpu: 'Процессор', ram: 'Память', swap: 'Swap', disk: 'Диск', inode: 'Inode' }, deliveryNames: { pending: 'В очереди', sent: 'Отправлено', failed: 'Ошибка', cancelled: 'Отменено' }, tabs: [{id:'settings',name:'Пороги и метрики'},{id:'channels',name:'Каналы доставки'},{id:'notifications',name:'Уведомления'},{id:'history',name:'История'}] }; },
  computed: {
    dirty() { return this.cfg && (JSON.stringify(this.cfg) !== this.original || this.mountText !== this.cfg.mounts.join(', ')); },
    paused() { return this.cfg && Date.parse(this.cfg.paused_until) > this.now; },
    stale() { return !this.status.snapshot?.at || this.now-Date.parse(this.status.snapshot.at) > (this.savedInterval*3+30)*1000; },
    healthClass() { if(this.stale || this.status.error || this.status.snapshot?.errors?.length) return 'warning'; const levels=Object.values(this.status.states || {}).map(s=>s.level);return levels.includes('critical')?'critical':levels.includes('warning')?'warning':'ok'; },
    healthLabel() { return this.stale?'Нет свежих данных':this.status.error || this.status.snapshot?.errors?.length?'Ошибка мониторинга':this.healthClass==='critical'?'Критическое состояние':this.healthClass==='warning'?'Требует внимания':'Система в норме'; }
  },
  methods: {
    async api(path, method='GET', body) { const res=await fetch('/api/'+path,{method,headers:{'Content-Type':'application/json','X-Watchdog-Request':'1'},body:body===undefined?undefined:JSON.stringify(body)});const data=await res.json();if(!res.ok){if(res.status===401)this.authenticated=false;throw new Error(data.error || 'Ошибка запроса');}return data; },
    acceptSettings(cfg) {this.cfg=cfg;this.original=JSON.stringify(cfg);this.mountText=cfg.mounts.join(', ');this.savedInterval=cfg.interval;},
    async initialize() {this.status=await this.api('status');this.acceptSettings(await this.api('settings'));this.authenticated=true;await this.loadHistory();},
    async login() {this.busy=true;this.error='';try{await this.api('login','POST',{password:this.password});this.password='';await this.initialize();}catch(e){this.error=e.message;}finally{this.busy=false;}},
    async logout() {try{await this.api('logout','POST',{});this.authenticated=false;this.cfg=null;this.error='';this.message='';}catch(e){this.error=e.message;}},
    async refresh() {if(!this.authenticated || this.refreshing)return;this.refreshing=true;try{this.status=await this.api('status');if(this.tab==='history')await this.loadHistory();}catch(e){this.error=e.message;}finally{this.refreshing=false;}},
    async save() {this.busy=true;this.error='';this.message='';try{const candidate=JSON.parse(JSON.stringify(this.cfg));candidate.mounts=this.mountText.split(',').map(s=>s.trim()).filter(Boolean);this.acceptSettings(await this.api('settings','PUT',candidate));this.message='Настройки сохранены. Правила применятся при следующем измерении.';}catch(e){this.error=e.message;}finally{this.busy=false;}},
    resetSettings(){this.acceptSettings(JSON.parse(this.original));this.error='';this.message='';},
    pause(hours){this.cfg.paused_until=new Date(Date.now()+hours*3600000).toISOString();},
    async test(channel){this.busy=true;this.error='';this.message='';try{const result=await this.api('test','POST',{channel});this.message=result.message;await this.loadHistory();}catch(e){this.error=e.message;}finally{this.busy=false;}},
    async loadHistory(){try{this.history=await this.api('history');}catch(e){this.error=e.message;}},
    date(value){if(!value || value.startsWith('0001'))return '—';return new Date(value).toLocaleString('ru-RU');},
    metricName(m){return this.names[m.kind]+(m.key.includes(':')?' · '+m.key.split(':').slice(1).join(':'):'');},
    metricLevel(m){return this.status.states?.[m.key]?.level || 'ok';},
    levelName(level){return {ok:'В норме',warning:'Предупреждение',critical:'Критично',test:'Тест'}[level] || level;},
    deliveries(id){return this.history.deliveries.filter(d=>d.event_id===id);}
  },
  async mounted(){try{await this.initialize();}catch(e){if(this.authenticated)this.error=e.message;}this.timer=setInterval(()=>{this.now=Date.now();this.refresh();},5000);},
  beforeUnmount(){clearInterval(this.timer);}
}).mount('#app');
