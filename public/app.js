const CONFIG = {
    SYMBOL: 'BTC_USDT',
    WS_URL: `ws://${window.location.host}/ws/market`,
    API_URL: `${window.location.origin}`,
    API_KEY: 'ultra-secret-key',
    USER_ID: parseInt(localStorage.getItem('USER_ID')) || null
};

const MARKET_DEFAULTS = {
    'BTC_USDT': { price: 68500, qty: 1 },
    'ETH_USDT': { price: 3450, qty: 10 },
    'SOL_USDT': { price: 145, qty: 100 }
};

class ExchangeUI {
    constructor() {
        this.socket = null;
        this.orderSide = 'buy';
        this.bids = [];
        this.asks = [];
        this.balances = {};

        // Data buckets
        this.allOrders = [];     // Every order from Redis history
        this.tradeExecutions = []; // Fragmented matches

        this.init();
    }

    async init() {
        console.log("Initializing Exchange UI...");
        this.bindEvents();

        if (!CONFIG.USER_ID) {
            document.getElementById('login-modal').classList.add('show');
            return;
        }

        this.updateProfileBadge();

        // Initial load
        await this.loadData();
        this.connectWS();
    }

    bindEvents() {
        // Order form
        document.getElementById('buy-tab').onclick = () => this.setSide('buy');
        document.getElementById('sell-tab').onclick = () => this.setSide('sell');
        document.getElementById('place-order-btn').onclick = () => this.placeOrder();
        document.getElementById('deposit-btn').onclick = () => this.mockDeposit();

        // 3-Tab Bottom Panel
        const tabMappings = {
            'tab-open': 'open-orders-view',
            'tab-order-hist': 'order-hist-view',
            'tab-trade-hist': 'trade-hist-view'
        };

        Object.keys(tabMappings).forEach(btnId => {
            const btn = document.getElementById(btnId);
            if (btn) {
                btn.onclick = () => {
                    console.log("Switching to tab:", btnId);
                    // Reset all
                    Object.keys(tabMappings).forEach(id => {
                        document.getElementById(id).classList.remove('active');
                        document.getElementById(tabMappings[id]).style.display = 'none';
                    });
                    // Activate selected
                    btn.classList.add('active');
                    document.getElementById(tabMappings[btnId]).style.display = 'block';
                };
            }
        });

        // Cancel order — event delegation on the tbody so it survives innerHTML re-renders.
        document.getElementById('open-orders-body').addEventListener('click', e => {
            const btn = e.target.closest('.cancel-order-btn');
            if (!btn) return;
            const orderId = btn.getAttribute('data-order-id'); // MUST NOT use parseInt, exceeds JS 53-bit int max
            const symbol = btn.getAttribute('data-symbol');
            this.cancelOrder(orderId, symbol);
        });

        // Market selector
        const dropdown = document.getElementById('market-dropdown');
        const dropBtn = document.getElementById('current-pair-btn');
        dropBtn.onclick = (e) => { e.stopPropagation(); dropdown.classList.toggle('show'); };
        dropdown.addEventListener('click', (e) => {
            const link = e.target.closest('a');
            if (link) {
                this.switchSymbol(link.getAttribute('data-symbol'));
                dropdown.classList.remove('show');
            }
        });
        document.addEventListener('click', () => dropdown.classList.remove('show'));

        // Real-time total calculation
        const priceInput = document.getElementById('order-price');
        const qtyInput = document.getElementById('order-qty');
        const updateT = () => {
            const orderType = document.getElementById('order-type') ? document.getElementById('order-type').value : 'LIMIT';
            if (orderType === 'MARKET') {
                document.getElementById('order-total').innerText = `Market Price`;
            } else {
                const t = (parseFloat(priceInput.value) || 0) * (parseFloat(qtyInput.value) || 0);
                document.getElementById('order-total').innerText = `${t.toLocaleString()} USDT`;
            }
        };
        priceInput.oninput = updateT;
        qtyInput.oninput = updateT;
        if (document.getElementById('order-type')) {
            document.getElementById('order-type').addEventListener('change', updateT);
        }

        // Login features
        const loginBtn = document.getElementById('login-submit-btn');
        if (loginBtn) {
            loginBtn.onclick = async () => {
                const uid = parseInt(document.getElementById('login-user-id').value);
                if (uid > 0) {
                    localStorage.setItem('USER_ID', uid);
                    CONFIG.USER_ID = uid;
                    document.getElementById('login-modal').classList.remove('show');
                    this.updateProfileBadge();
                    await this.loadData();
                    if (!this.socket) {
                        this.connectWS();
                    }
                }
            };
        }

        const profileBtn = document.getElementById('profile-btn');
        if (profileBtn) {
            profileBtn.onclick = () => {
                document.getElementById('login-user-id').value = CONFIG.USER_ID || '';
                document.getElementById('login-modal').classList.add('show');
            };
        }

        const orderTypeSelect = document.getElementById('order-type');
        const priceGroup = document.getElementById('price-group');
        const advGroup = document.getElementById('advanced-options-group');

        if (orderTypeSelect) {
            orderTypeSelect.onchange = (e) => {
                if (e.target.value === 'MARKET') {
                    priceGroup.style.display = 'none';
                    advGroup.style.display = 'none';
                } else {
                    priceGroup.style.display = 'block';
                    advGroup.style.display = 'block';
                }
            };
        }
    }

    updateProfileBadge() {
        document.getElementById('user-id-badge').innerText = `User: ${CONFIG.USER_ID}`;
    }

    async loadData() {
        // Reset local structures
        this.allOrders = [];
        this.tradeExecutions = [];
        this.bids = [];
        this.asks = [];
        document.getElementById('trade-history').innerHTML = '';

        await Promise.all([
            this.refreshBalances(),
            this.syncOrders(),
            this.syncTrades()
        ]);

        this.setMarketDefaults(CONFIG.SYMBOL);
        this.fetchInitialDepth();
    }

    async refreshBalances() {
        try {
            const res = await fetch(`${CONFIG.API_URL}/balances?user_id=${CONFIG.USER_ID}`, {
                headers: { 'X-API-KEY': CONFIG.API_KEY }
            });
            if (res.ok) {
                this.balances = await res.json();
                this.updateBalanceUI();
            }
        } catch (e) { console.error("Balance fetch failed", e); }
    }

    async syncOrders() {
        try {
            const res = await fetch(`${CONFIG.API_URL}/orders?user_id=${CONFIG.USER_ID}`, {
                headers: { 'X-API-KEY': CONFIG.API_KEY }
            });
            if (res.ok) {
                const data = await res.json();
                this.allOrders = (data || []).map(o => ({
                    id: o.id,
                    time: new Date(o.timestamp / 1000000).toLocaleTimeString('en-GB', { hour12: false }),
                    symbol: o.symbol || CONFIG.SYMBOL,
                    side: o.side === 0 ? 'buy' : 'sell',
                    price: o.price,
                    initialQty: o.initial_qty,
                    filledQty: o.initial_qty - o.qty,
                    status: o.status || (o.qty === 0 ? 'FILLED' : 'OPEN')
                })).sort((a, b) => b.id - a.id);
                this.renderAllViews();
            }
        } catch (e) { console.error("Order sync failed", e); }
    }

    async syncTrades() {
        try {
            const res = await fetch(`${CONFIG.API_URL}/trades?user_id=${CONFIG.USER_ID}`, {
                headers: { 'X-API-KEY': CONFIG.API_KEY }
            });
            if (res.ok) {
                const data = await res.json() || [];
                this.tradeExecutions = data.map(t => ({
                    time: new Date(t.time / 1000000).toLocaleTimeString('en-GB', { hour12: false }),
                    symbol: t.symbol,
                    price: t.price,
                    qty: t.qty,
                    side: t.side === 0 ? 'buy' : 'sell',
                    role: t.role
                }));
                this.renderAllViews();
            }
        } catch (e) { console.error("Trade history sync failed", e); }
    }

    async fetchInitialDepth() {
        try {
            const res = await fetch(`${CONFIG.API_URL}/depth?symbol=${CONFIG.SYMBOL}`, {
                headers: { 'X-API-KEY': CONFIG.API_KEY }
            });
            const data = await res.json();
            this.processSnapshot(data);
            this.renderOrderbook();
        } catch (e) { }
    }

    processSnapshot(data) {
        if (!data) return;
        const bidsMap = {};
        const asksMap = {};
        (data.orders || []).forEach(o => {
            const map = o.side === 0 ? bidsMap : asksMap;
            map[o.price] = (map[o.price] || 0) + o.qty;
        });
        this.bids = Object.entries(bidsMap).map(([p, q]) => ({ price: parseInt(p), qty: q })).sort((a, b) => b.price - a.price);
        this.asks = Object.entries(asksMap).map(([p, q]) => ({ price: parseInt(p), qty: q })).sort((a, b) => a.price - b.price);
    }

    connectWS() {
        // Pass the current symbol as initial subscription via query param
        const url = `${CONFIG.WS_URL}?symbol=${CONFIG.SYMBOL}`;
        this.socket = new WebSocket(url);

        this.socket.onopen = () => {
            // Also send a subscribe control frame so the server registers us
            // (backend supports both query param + JSON control frames)
            this.socket.send(JSON.stringify({ action: 'subscribe', symbol: CONFIG.SYMBOL }));
            console.log(`[WS] Subscribed to ${CONFIG.SYMBOL}`);
        };

        this.socket.onmessage = (event) => {
            const msg = JSON.parse(event.data);
            if (msg.type === 'match') {
                this.handleMatches(msg.data, msg.symbol);
            } else if (msg.type === 'depth') {
                this.handleDepthUpdate(msg.data, msg.symbol);
            }
        };
        this.socket.onclose = () => setTimeout(() => this.connectWS(), 2000);
    }

    handleDepthUpdate(depth, symbol) {
        if (symbol !== CONFIG.SYMBOL) return;
        this.bids = depth.bids;
        this.asks = depth.asks;
        this.renderOrderbook();
    }

    handleMatches(match, symbol) {
        if (symbol === CONFIG.SYMBOL) this.addTradeToMarketHistory(match);

        let interest = false;
        const isMaker = match.MakerUserID === CONFIG.USER_ID;
        const isTaker = match.TakerUserID === CONFIG.USER_ID;

        if (isMaker || isTaker) {
            interest = true;
            const time = new Date().toLocaleTimeString('en-GB', { hour12: false });

            // Record separate execution events for trade history
            if (isMaker) this.tradeExecutions.unshift({ time, symbol, price: match.Price, qty: match.Qty, side: match.TakerSide === 0 ? 'sell' : 'buy', role: 'MAKER' });
            if (isTaker) this.tradeExecutions.unshift({ time, symbol, price: match.Price, qty: match.Qty, side: match.TakerSide === 0 ? 'buy' : 'sell', role: 'TAKER' });

            // Update order summaries in allOrders
            const ids = [match.MakerOrderID, match.TakerOrderID];
            ids.forEach(id => {
                const order = this.allOrders.find(o => o.id === id);
                if (order) {
                    order.filledQty += match.Qty;
                    if (order.filledQty >= order.initialQty) order.status = 'FILLED';
                }
            });
        }

        if (interest) {
            this.refreshBalances();
            this.renderAllViews();
        }
    }

    renderAllViews() {
        // Shared renderer for Order History tab (no cancel button)
        const renderHistRow = o => `
            <tr>
                <td style="color: var(--text-dim)">${o.time}</td>
                <td style="font-weight: 600">${o.symbol}</td>
                <td class="${o.side === 'buy' ? 'bid' : 'ask'}">${o.side.toUpperCase()}</td>
                <td style="font-family: var(--font-mono)">${o.price.toLocaleString()}</td>
                <td style="font-family: var(--font-mono)">${o.filledQty.toFixed(2)}</td>
                <td style="font-family: var(--font-mono)">${o.initialQty.toFixed(2)}</td>
                <td class="${o.status === 'FILLED' ? 'status-filled' : o.status === 'CANCELED' ? 'status-canceled' : 'status-open'}">${o.status}</td>
            </tr>
        `;

        // Open orders renderer: extra Action column with Cancel button
        const renderOpenRow = o => `
            <tr>
                <td style="color: var(--text-dim)">${o.time}</td>
                <td style="font-weight: 600">${o.symbol}</td>
                <td class="${o.side === 'buy' ? 'bid' : 'ask'}">${o.side.toUpperCase()}</td>
                <td style="font-family: var(--font-mono)">${o.price.toLocaleString()}</td>
                <td style="font-family: var(--font-mono)">${o.filledQty.toFixed(2)}</td>
                <td style="font-family: var(--font-mono)">${o.initialQty.toFixed(2)}</td>
                <td class="status-open">OPEN</td>
                <td>
                    <button
                        class="cancel-order-btn"
                        data-order-id="${o.id}"
                        data-symbol="${o.symbol}"
                        title="Cancel Order #${o.id}"
                    >Cancel</button>
                </td>
            </tr>
        `;

        // Tab 1: Open orders only
        document.getElementById('open-orders-body').innerHTML = this.allOrders
            .filter(o => o.status === 'OPEN')
            .map(renderOpenRow).join('');

        // Tab 2: All orders (no action column)
        document.getElementById('order-hist-body').innerHTML = this.allOrders
            .map(renderHistRow).join('');

        // Tab 3: Trade executions
        document.getElementById('trade-hist-body').innerHTML = this.tradeExecutions.slice(0, 50).map(t => `
            <tr>
                <td style="color: var(--text-dim)">${t.time}</td>
                <td style="font-weight: 600">${t.symbol}</td>
                <td class="${t.side === 'buy' ? 'bid' : 'ask'}">${t.side.toUpperCase()}</td>
                <td style="font-family: var(--font-mono)">${t.price.toLocaleString()}</td>
                <td style="font-family: var(--font-mono)">${t.qty.toFixed(4)}</td>
                <td style="color: #aaa; font-size: 0.75rem;">${t.role}</td>
                <td style="font-size: 0.75rem;">0.00%</td>
            </tr>
        `).join('');
    }

    async placeOrder() {
        const p = parseInt(document.getElementById('order-price').value);
        const q = parseInt(document.getElementById('order-qty').value);
        const orderType = document.getElementById('order-type') ? document.getElementById('order-type').value : 'LIMIT';
        const tif = document.getElementById('time-in-force') ? document.getElementById('time-in-force').value : 'GTC';
        const postOnly = document.getElementById('post-only') ? document.getElementById('post-only').checked : false;

        if (!q) return;
        if (orderType === 'LIMIT' && !p) return;

        try {
            const res = await fetch(`${CONFIG.API_URL}/orders`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json', 'X-API-KEY': CONFIG.API_KEY },
                body: JSON.stringify({
                    user_id: CONFIG.USER_ID,
                    price: p || 0,
                    qty: q,
                    side: this.orderSide,
                    symbol: CONFIG.SYMBOL,
                    type: orderType,
                    time_in_force: tif,
                    post_only: postOnly
                })
            });

            if (res.ok) {
                const data = await res.json();
                const remaining = data.remaining_qty !== undefined ? data.remaining_qty : q;
                const filledQty = q - remaining;
                const status = remaining === 0 ? 'FILLED' : 'OPEN';

                this.allOrders.unshift({
                    id: data.order_id,
                    time: new Date().toLocaleTimeString('en-GB', { hour12: false }),
                    symbol: CONFIG.SYMBOL,
                    side: this.orderSide,
                    price: p || 0,
                    initialQty: q,
                    filledQty: filledQty,
                    status: status
                });
                this.renderAllViews();
                this.refreshBalances();
                this.showToast('✅ Order Placed Successfully!');
            } else {
                const errData = await res.json();
                this.showToast(`❌ Error: ${errData.error || 'Failed to place order'}`);
            }
        } catch (e) {
            this.showToast(`❌ Connection Error!`);
        }
    }

    async cancelOrder(orderId, symbol) {
        const btn = document.querySelector(`.cancel-order-btn[data-order-id="${orderId}"]`);
        if (btn) {
            btn.disabled = true;
            btn.innerText = '...';
        }
        try {
            const res = await fetch(`${CONFIG.API_URL}/orders`, {
                method: 'DELETE',
                headers: { 'Content-Type': 'application/json', 'X-API-KEY': CONFIG.API_KEY },
                body: JSON.stringify({ order_id: orderId, symbol: symbol })
            });
            if (res.ok) {
                // Mark locally so the row disappears immediately without waiting for next sync
                const order = this.allOrders.find(o => o.id === orderId);
                if (order) order.status = 'CANCELED';
                this.renderAllViews();
                this.refreshBalances();
                this.showToast(`🗑️ Order #${orderId} Cancelled`);
            } else {
                const errData = await res.json();
                this.showToast(`❌ Cancel failed: ${errData.error || 'Unknown error'}`);
                if (btn) { btn.disabled = false; btn.innerText = 'Cancel'; }
            }
        } catch (e) {
            this.showToast('❌ Connection Error!');
            if (btn) { btn.disabled = false; btn.innerText = 'Cancel'; }
        }
    }

    showToast(msg) {
        const area = document.getElementById('notification-area');
        if (!area) return;
        const div = document.createElement('div');
        div.className = 'toast';
        div.innerText = msg;

        area.appendChild(div);

        // Remove styling after animation
        setTimeout(() => {
            div.style.opacity = '0';
            div.style.transform = 'translateX(100px)';
            div.style.transition = 'all 0.4s ease';
            setTimeout(() => div.remove(), 400);
        }, 3000);
    }

    addTradeToMarketHistory(match) {
        const container = document.getElementById('trade-history');
        const row = document.createElement('div');
        const isBid = match.TakerSide === 0;
        row.className = `trade-row ${isBid ? 'bid' : 'ask'}`;
        row.innerHTML = `<span class="${isBid ? 'price-up' : 'price-down'}">${match.Price.toLocaleString()}</span><span>${match.Qty.toFixed(4)}</span><span class="time">${new Date().toLocaleTimeString('en-GB', { hour12: false })}</span>`;
        container.prepend(row);
        if (container.children.length > 50) container.lastChild.remove();
        document.getElementById('top-price').innerText = match.Price.toLocaleString();
    }

    renderOrderbook() {
        const asksContainer = document.getElementById('asks');
        const bidsContainer = document.getElementById('bids');
        if (!asksContainer || !bidsContainer) return;

        // Show more levels for a professional look (Binance-style)
        const displayLimit = 30;
        const visibleAsks = this.asks.slice(0, displayLimit);
        const visibleBids = this.bids.slice(0, displayLimit);

        const maxQty = Math.max(
            ...visibleAsks.map(a => a.qty),
            ...visibleBids.map(b => b.qty),
            1
        );

        // Update Spread UI
        if (visibleAsks.length > 0 && visibleBids.length > 0) {
            const bestAsk = visibleAsks[0].price;
            const bestBid = visibleBids[0].price;
            const spread = bestAsk - bestBid;
            const spreadPct = (spread / bestAsk) * 100;

            const spreadPriceEl = document.getElementById('spread-price');
            const spreadPercentEl = document.getElementById('spread-percent');

            if (spreadPriceEl) spreadPriceEl.innerText = bestBid.toLocaleString();
            if (spreadPercentEl) spreadPercentEl.innerText = `${spread < 0 ? 0 : spreadPct.toFixed(2)}%`;
        }

        // Render Asks (Reverse so they grow up from the spread)
        asksContainer.innerHTML = visibleAsks.map(a => `
            <div class="book-row" onclick="document.getElementById('order-price').value='${a.price}'">
                <span class="price">${a.price.toLocaleString()}</span>
                <span>${a.qty.toFixed(4)}</span>
                <span style="color: var(--text-dim)">${(a.price * a.qty / 1000).toFixed(1)}k</span>
                <div class="depth-bar" style="width: ${(a.qty / maxQty * 100)}%"></div>
            </div>
        `).join('');

        // Render Bids
        bidsContainer.innerHTML = visibleBids.map(b => `
            <div class="book-row" onclick="document.getElementById('order-price').value='${b.price}'">
                <span class="price">${b.price.toLocaleString()}</span>
                <span>${b.qty.toFixed(4)}</span>
                <span style="color: var(--text-dim)">${(b.price * b.qty / 1000).toFixed(1)}k</span>
                <div class="depth-bar" style="width: ${(b.qty / maxQty * 100)}%"></div>
            </div>
        `).join('');
    }

    async mockDeposit() {
        const b = CONFIG.SYMBOL.split('_')[0];
        await Promise.all([
            fetch(`${CONFIG.API_URL}/deposit?user_id=${CONFIG.USER_ID}&asset=USDT&amount=1000000`, { method: 'POST', headers: { 'X-API-KEY': CONFIG.API_KEY } }),
            fetch(`${CONFIG.API_URL}/deposit?user_id=${CONFIG.USER_ID}&asset=${b}&amount=1000`, { method: 'POST', headers: { 'X-API-KEY': CONFIG.API_KEY } })
        ]);
        this.refreshBalances();
    }

    updateBalanceUI() {
        const b = CONFIG.SYMBOL.split('_')[0];
        document.getElementById('bal-btc').innerText = (this.balances[b] || 0).toLocaleString();
        document.getElementById('bal-usdt').innerText = (this.balances['USDT'] || 0).toLocaleString();
    }

    setSide(side) {
        this.orderSide = side;
        document.getElementById('buy-tab').classList.toggle('active', side === 'buy');
        document.getElementById('sell-tab').classList.toggle('active', side === 'sell');
        const btn = document.getElementById('place-order-btn');
        btn.className = side === 'buy' ? 'buy-btn' : 'sell-btn';
        btn.innerText = `Place ${side.charAt(0).toUpperCase() + side.slice(1)} Order`;
    }

    setMarketDefaults(symbol) {
        const def = MARKET_DEFAULTS[symbol];
        if (def) {
            document.getElementById('order-price').value = def.price;
            document.getElementById('order-qty').value = def.qty;
            document.getElementById('order-total').innerText = `${(def.price * def.qty).toLocaleString()} USDT`;
        }
    }

    switchSymbol(symbol) {
        const prev = CONFIG.SYMBOL;
        CONFIG.SYMBOL = symbol;
        document.getElementById('current-pair-btn').innerHTML = `${symbol} <span class="arrow">▼</span>`;

        // Resubscribe WebSocket to the new symbol
        if (this.socket && this.socket.readyState === WebSocket.OPEN) {
            this.socket.send(JSON.stringify({ action: 'unsubscribe', symbol: prev }));
            this.socket.send(JSON.stringify({ action: 'subscribe', symbol: symbol }));
            console.log(`[WS] Switched subscription from ${prev} to ${symbol}`);
        }

        this.loadData();
    }
}
window.onload = () => new ExchangeUI();
