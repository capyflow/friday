package com.capyflow.friday.mobile;

import android.annotation.SuppressLint;
import android.net.http.SslError;
import android.os.Bundle;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.JavascriptInterface;
import android.webkit.SslErrorHandler;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import androidx.activity.OnBackPressedCallback;
import androidx.appcompat.app.AlertDialog;
import androidx.appcompat.app.AppCompatActivity;

import com.journeyapps.barcodescanner.ScanContract;
import com.journeyapps.barcodescanner.ScanOptions;

import org.json.JSONObject;

import java.security.MessageDigest;
import java.security.SecureRandom;
import java.security.cert.X509Certificate;

/**
 * Friday Mobile 壳：极薄 WebView，加载内核的移动端 PWA（/mobile）。
 * 壳只负责四件浏览器做不到的事——记住服务端地址、TOFU 信任自签证书、
 * 服务器不在线时给一张人话错误页、扫码（把桌面端二维码带回来）。
 * 大脑与界面全部在 Go 内核与 web/src/mobile（见 README「手机端」）。
 *
 * 证书策略：连接时校验服务器叶子证书 SHA-256 指纹——扫码进来的指纹随二维码
 * 一起拿到（电脑屏幕属带外可信通道），直接 pin；手输地址走 TOFU，首次连接
 * 展示指纹人工确认一次。电脑换网重签证书后指纹变化，会再确认一次（预期）。
 */
public class MainActivity extends AppCompatActivity {

    private static final String PREFS = "friday_mobile";
    private static final String KEY_ADDR = "server_addr";
    private static final String KEY_FP_PREFIX = "pinned_fp_";
    /** 供网页调用的原生桥名：window.FridayNative（见 web/src/mobile/native.js）。 */
    private static final String BRIDGE = "FridayNative";

    private android.content.SharedPreferences prefs;

    private FrameLayout root;
    private LinearLayout connectView;
    private LinearLayout offlineView;
    private TextView offlineAddr;
    private TextView offlineDetail;
    private EditText addrInput;
    private WebView webView;
    private boolean webReady = false;
    private volatile String currentOrigin = null;
    /** 本次扫码是否由网页发起（网页要拿回扫描结果自己解析）。 */
    private boolean scanForWeb = false;
    /** TLS 探测指纹缓存（host → sha256）：一次连接内避免反复握手。 */
    private final java.util.Map<String, String> fpCache = new java.util.concurrent.ConcurrentHashMap<>();

    private final androidx.activity.result.ActivityResultLauncher<ScanOptions> scanLauncher =
            registerForActivityResult(new ScanContract(), result -> {
                String text = result.getContents();
                if (scanForWeb) {
                    // 网页发起的扫码：结果交回网页自己解析（配对链接 / 纯地址）
                    scanForWeb = false;
                    notifyScanResult(text == null ? "" : text);
                    return;
                }
                if (text != null) handleScannedUrl(text);
            });

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        prefs = getSharedPreferences(PREFS, MODE_PRIVATE);

        // debug 包开放 WebView CDP（chrome://inspect / adb forward），方便联调
        if (BuildConfig.DEBUG) {
            WebView.setWebContentsDebuggingEnabled(true);
        }

        root = new FrameLayout(this);
        setContentView(root);

        buildConnectView();
        buildOfflineView();
        buildWebView();

        root.addView(connectView, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        root.addView(webView, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        root.addView(offlineView, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        String saved = prefs.getString(KEY_ADDR, null);
        if (saved != null) {
            openWebView(saved, null);
        } else {
            showConnect();
        }

        getOnBackPressedDispatcher().addCallback(this, new OnBackPressedCallback(true) {
            @Override
            public void handleOnBackPressed() {
                if (webReady && webView.canGoBack()) {
                    webView.goBack();
                } else {
                    moveTaskToBack(true); // 驻留：回桌面不销毁，登录态保留在 WebView
                }
            }
        });
    }

    /* ---------- 视图切换 ---------- */

    private void showConnect() {
        webReady = false;
        connectView.setVisibility(View.VISIBLE);
        webView.setVisibility(View.GONE);
        offlineView.setVisibility(View.GONE);
    }

    /**
     * 服务器不在线的人话页：Chromium 自带的 net::ERR_CONNECTION_REFUSED 错误页
     * 对用户没有意义（还带一串英文错误码），这里用原生页替掉——说清「服务器不在线」
     * 与怎么修，并给重试 / 换地址两个出口。
     */
    private void showOffline(String reason) {
        webReady = false;
        connectView.setVisibility(View.GONE);
        webView.setVisibility(View.GONE);
        offlineView.setVisibility(View.VISIBLE);
        if (offlineDetail != null) {
            offlineDetail.setText(reason == null || reason.isEmpty()
                    ? "没能连上这台电脑上的 Friday。请确认电脑端正在运行、"
                    + "手机与电脑连的是同一个 Wi-Fi、地址没写错。"
                    : reason);
        }
    }

    /* ---------- 连接视图 ---------- */

    private void buildConnectView() {
        connectView = new LinearLayout(this);
        connectView.setOrientation(LinearLayout.VERTICAL);
        connectView.setPadding(dp(28), dp(80), dp(28), dp(28));
        connectView.setGravity(Gravity.CENTER_HORIZONTAL);
        connectView.setVisibility(View.GONE);

        TextView brand = new TextView(this);
        brand.setText("小五");
        brand.setTextSize(30);
        brand.setGravity(Gravity.CENTER);
        connectView.addView(brand);

        TextView sub = new TextView(this);
        sub.setText("连接到电脑上的 Friday（同一 Wi-Fi）");
        sub.setTextSize(14);
        sub.setAlpha(0.65f);
        sub.setPadding(0, dp(6), 0, dp(32));
        connectView.addView(sub);

        addrInput = new EditText(this);
        addrInput.setHint("192.168.10.242:45679");
        addrInput.setInputType(android.text.InputType.TYPE_TEXT_VARIATION_URI);
        addrInput.setSingleLine(true);
        addrInput.setTextSize(16); // ≥16 免聚焦自动放大
        connectView.addView(addrInput, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(0, dp(18), 0, 0);
        LinearLayout.LayoutParams rowLp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);

        Button connect = new Button(this);
        connect.setText("连接");
        connect.setOnClickListener(v -> connect());
        row.addView(connect, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        Button scan = new Button(this);
        scan.setText("扫码配对");
        scan.setOnClickListener(v -> startScan());
        LinearLayout.LayoutParams scanLp = new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
        scanLp.setMarginStart(dp(10));
        row.addView(scan, scanLp);

        connectView.addView(row, rowLp);

        TextView foot = new TextView(this);
        foot.setText("服务端地址 = 电脑的局域网 IP + 局域网门端口\n（电脑端「设置 → 手机连接」可查看）");
        foot.setTextSize(12);
        foot.setAlpha(0.55f);
        foot.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams footLp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        footLp.topMargin = dp(28);
        connectView.addView(foot, footLp);
    }

    /* ---------- 离线页 ---------- */

    private void buildOfflineView() {
        offlineView = new LinearLayout(this);
        offlineView.setOrientation(LinearLayout.VERTICAL);
        offlineView.setPadding(dp(28), dp(80), dp(28), dp(28));
        offlineView.setGravity(Gravity.CENTER_HORIZONTAL);
        offlineView.setVisibility(View.GONE);

        TextView title = new TextView(this);
        title.setText("服务器不在线");
        title.setTextSize(26);
        title.setGravity(Gravity.CENTER);
        offlineView.addView(title);

        offlineAddr = new TextView(this);
        offlineAddr.setTextSize(13);
        offlineAddr.setAlpha(0.6f);
        offlineAddr.setGravity(Gravity.CENTER);
        offlineAddr.setPadding(0, dp(10), 0, dp(18));
        offlineView.addView(offlineAddr);

        offlineDetail = new TextView(this);
        offlineDetail.setTextSize(15);
        offlineDetail.setAlpha(0.85f);
        offlineDetail.setGravity(Gravity.CENTER);
        offlineDetail.setLineSpacing(dp(4), 1f);
        offlineView.addView(offlineDetail, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(0, dp(28), 0, 0);
        LinearLayout.LayoutParams rowLp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);

        Button retry = new Button(this);
        retry.setText("重试连接");
        retry.setOnClickListener(v -> {
            String origin = currentOrigin != null ? currentOrigin : prefs.getString(KEY_ADDR, null);
            if (origin == null) {
                showConnect();
            } else {
                openWebView(origin, null);
            }
        });
        row.addView(retry, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        Button change = new Button(this);
        change.setText("换个地址");
        change.setOnClickListener(v -> {
            String origin = currentOrigin != null ? currentOrigin : prefs.getString(KEY_ADDR, null);
            addrInput.setText(origin == null ? "" : origin.replaceFirst("^https://", ""));
            showConnect();
        });
        LinearLayout.LayoutParams changeLp = new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
        changeLp.setMarginStart(dp(10));
        row.addView(change, changeLp);

        offlineView.addView(row, rowLp);
    }

    private void connect() {
        String addr = normalizeAddr(addrInput.getText().toString());
        if (addr == null) {
            toast("地址无效：填 电脑IP:端口（例 192.168.10.242:45679）");
            return;
        }
        prefs.edit().putString(KEY_ADDR, addr).apply();
        openWebView(addr, null);
    }

    /** 规范化：缺 scheme 补 https://；只接受 https；返回 origin（scheme://host:port）。 */
    private String normalizeAddr(String raw) {
        String s = raw == null ? "" : raw.trim();
        if (s.isEmpty()) return null;
        if (!s.contains("://")) s = "https://" + s;
        android.net.Uri uri = android.net.Uri.parse(s);
        if (!"https".equals(uri.getScheme()) || uri.getHost() == null) return null;
        return uri.getScheme() + "://" + uri.getAuthority();
    }

    /* ---------- 扫码配对 ---------- */

    private void startScan() {
        ScanOptions opt = new ScanOptions()
                .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                .setPrompt("扫描电脑端「手机连接」二维码")
                .setBeepEnabled(false)
                .setOrientationLocked(true);
        scanLauncher.launch(opt);
    }

    /**
     * 桌面端二维码两种内容都吃：
     * ① 配对链接 https://host:port/mobile?pair=CODE&fp=FINGERPRINT → 直接开它
     *   （指纹随码校验、免 TOFU；配对码由网页 redeem 换 JWT）。
     * ② 纯地址文本（host:port 或 https://host:port）→ 当服务端地址连。
     */
    private void handleScannedUrl(String raw) {
        String s = raw == null ? "" : raw.trim();
        if (s.isEmpty()) return;
        if (!s.contains("://")) {
            String addr = normalizeAddr(s);
            if (addr == null) {
                toast("二维码里不是服务器地址");
                return;
            }
            prefs.edit().putString(KEY_ADDR, addr).apply();
            openWebView(addr, null);
            return;
        }
        android.net.Uri uri = android.net.Uri.parse(s);
        if (!"https".equals(uri.getScheme()) || uri.getHost() == null) {
            toast("二维码不是 Friday 配对码");
            return;
        }
        connectToUrl(s);
    }

    /**
     * 连到某个地址：记住所址、指纹随码 pin（带外可信通道），再打开目标 URL。
     * 带 ?pair= 的走原链接（网页端 redeem 换 JWT）；不带的一律开 /mobile。
     *
     * 方法名带 Url 后缀：内层 NativeBridge 也有 connectTo(String)（暴露给网页的
     * 桥方法），同名会遮蔽本方法——桥里那层调本层就成了无限递归。
     */
    private void connectToUrl(String raw) {
        String s = raw == null ? "" : raw.trim();
        if (s.isEmpty()) return;
        if (!s.contains("://")) s = "https://" + s;
        android.net.Uri uri = android.net.Uri.parse(s);
        if (!"https".equals(uri.getScheme()) || uri.getHost() == null) {
            toast("地址无效：填 电脑IP:端口（例 192.168.10.242:45679）");
            return;
        }
        String origin = uri.getScheme() + "://" + uri.getAuthority();
        String fp = uri.getQueryParameter("fp");
        if (fp != null && fp.length() == 64) {
            pinFingerprint(origin, fp); // 扫码即带外可信通道：指纹随码校验，直接 pin
        }
        prefs.edit().putString(KEY_ADDR, origin).apply();

        String query = uri.getEncodedQuery();
        boolean pairing = query != null && query.contains("pair=");
        String full;
        if (pairing) {
            String path = uri.getPath() == null || uri.getPath().isEmpty() ? "/mobile" : uri.getPath();
            full = origin + path + (query == null ? "" : "?" + query);
        } else {
            full = origin + "/mobile";
        }
        openWebView(origin, full);
    }

    /* ---------- WebView ---------- */

    @SuppressLint("SetJavaScriptEnabled")
    private void buildWebView() {
        webView = new WebView(this);
        webView.getSettings().setJavaScriptEnabled(true);
        webView.getSettings().setDomStorageEnabled(true); // 登录态 localStorage
        webView.setVisibility(View.GONE);
        // 网页（移动端登录页）需要「当前服务器地址 / 切换服务器 / 扫码」三件浏览器做不到的事
        webView.addJavascriptInterface(new NativeBridge(), BRIDGE);
    }

    private void openWebView(String origin, String fullUrl) {
        currentOrigin = origin;
        webReady = false;
        connectView.setVisibility(View.GONE);
        offlineView.setVisibility(View.GONE);
        webView.setVisibility(View.VISIBLE);
        if (offlineAddr != null) {
            offlineAddr.setText(origin == null ? "" : origin.replaceFirst("^https://", ""));
        }
        webView.setWebViewClient(client());
        webView.loadUrl(fullUrl != null ? fullUrl : origin + "/mobile");
    }

    private WebViewClient client() {
        return new WebViewClient() {
            @Override
            public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
                // SslCertificate.saveState() 的 Bundle 黑科技在部分机型（实测三星）
                // 取不出证书字节 → 无法算指纹。改为后台对目标主机做一次真实 TLS
                // 探测拿叶子证书（trust-all 只为取字节，不作为信任依据），拿到
                // 指纹后再决定 pin/proceed 或弹 TOFU。探测期间 handler 挂起加载。
                android.net.Uri uri = android.net.Uri.parse(currentOrigin == null ? "" : currentOrigin);
                final String host = uri.getHost();
                final int port = uri.getPort() > 0 ? uri.getPort() : 443;
                if (host == null) {
                    handler.cancel();
                    return;
                }
                new Thread(() -> {
                    String fp = fpCache.containsKey(host) ? fpCache.get(host) : probeFingerprint(host, port);
                    if (fp != null) fpCache.put(host, fp);
                    String pinned = prefs.getString(KEY_FP_PREFIX + host, null);
                    boolean ok = fp != null && fp.equals(pinned);
                    runOnUiThread(() -> decideSsl(handler, host, fp, ok));
                }).start();
            }

            @Override
            public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
                // 只管主框架首屏失败（子资源失败不该把整页换成离线页）
                if (request == null || !request.isForMainFrame()) return;
                CharSequence desc = error == null ? null : error.getDescription();
                showOffline(desc == null || desc.isEmpty()
                        ? null
                        : "没能连接服务器：" + desc + "\n请确认电脑端正在运行、地址与 Wi-Fi 都没问题。");
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                webReady = true;
            }
        };
    }

    /** SSL 决策（UI 线程）：已 pin 指纹直接放行；否则弹 TOFU；拿不到证书拒绝。 */
    private void decideSsl(SslErrorHandler handler, String host, String fp, boolean pinnedOk) {
        if (pinnedOk) {
            handler.proceed();
            return;
        }
        if (fp == null) { // 探测失败（网络抖动等），宁可拒绝也不放行
            showOffline("读不出服务器的证书，多半是网络不稳或地址不对。可以换个地址再试。");
            handler.cancel();
            return;
        }
        new AlertDialog.Builder(this)
                .setTitle("是否信任此服务器？")
                .setMessage("证书指纹 (SHA-256)：\n" + groupFingerprint(fp)
                        + "\n\n请与电脑端「设置 → 手机连接」页核对一致后选择信任。"
                        + "\n（提示：实时通知依赖系统信任的证书，建议同时安装一次 CA："
                        + currentOrigin + "/friday-ca.crt）")
                .setPositiveButton("信任并继续", (d, w) -> {
                    pinFingerprint(currentOrigin, fp);
                    handler.proceed();
                })
                .setNegativeButton("取消", (d, w) -> handler.cancel())
                .setOnCancelListener(d -> handler.cancel())
                .show();
    }

    /**
     * 对 host:port 做一次真实 TLS 握手取叶子证书指纹。信任管理器放行一切——
     * 我们只要证书字节本身（指纹算完由人眼/扫码带外比对），不在这里做信任决策。
     * 3 秒超时，失败返回 null。
     */
    private String probeFingerprint(String host, int port) {
        javax.net.ssl.TrustManager[] trustAll = new javax.net.ssl.TrustManager[]{
                new javax.net.ssl.X509TrustManager() {
                    public void checkClientTrusted(X509Certificate[] c, String a) {}
                    public void checkServerTrusted(X509Certificate[] c, String a) {}
                    public X509Certificate[] getAcceptedIssuers() { return new X509Certificate[0]; }
                }
        };
        try {
            javax.net.ssl.SSLContext ctx = javax.net.ssl.SSLContext.getInstance("TLS");
            ctx.init(null, trustAll, new SecureRandom());
            javax.net.ssl.SSLSocketFactory f = ctx.getSocketFactory();
            try (javax.net.ssl.SSLSocket socket = (javax.net.ssl.SSLSocket) f.createSocket()) {
                socket.connect(new java.net.InetSocketAddress(host, port), 3000);
                socket.setSoTimeout(3000);
                socket.startHandshake();
                java.security.cert.Certificate[] chain = socket.getSession().getPeerCertificates();
                if (chain == null || chain.length == 0) return null;
                MessageDigest md = MessageDigest.getInstance("SHA-256");
                byte[] digest = md.digest(chain[0].getEncoded());
                StringBuilder sb = new StringBuilder();
                for (byte b : digest) sb.append(String.format("%02x", b));
                return sb.toString();
            }
        } catch (Exception e) {
            return null;
        }
    }

    private void pinFingerprint(String origin, String fp) {
        // key 必须与 onReceivedSslError 里的查询一致：getHost() 不含端口
        String host = android.net.Uri.parse(origin).getHost();
        prefs.edit().putString(KEY_FP_PREFIX + host, fp).apply();
    }

    private String groupFingerprint(String fp) {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < fp.length(); i += 2) {
            if (i > 0 && i % 8 == 0) sb.append('\n');
            else if (i > 0) sb.append(' ');
            sb.append(fp, i, Math.min(i + 2, fp.length()));
        }
        return sb.toString();
    }

    /* ---------- 原生桥（网页 → 壳） ---------- */

    /**
     * 移动端网页需要的三件浏览器做不到的事，交给壳做：
     * ① 取当前服务器地址（网页登录页回填输入框）；
     * ② 切换服务器（网页改了地址 → 壳换 origin 重载，证书信任与地址记忆都在壳侧）；
     * ③ 扫码（网页点图标 → 壳起原生扫码页 → 结果回填网页）。
     * 网页侧封装见 web/src/mobile/native.js；浏览器直访时该桥不存在，一律降级。
     */
    private class NativeBridge {

        @JavascriptInterface
        public boolean isApp() {
            return true;
        }

        @JavascriptInterface
        public String getServerAddress() {
            String o = currentOrigin != null ? currentOrigin : prefs.getString(KEY_ADDR, null);
            return o == null ? "" : o;
        }

        @JavascriptInterface
        public void setServerAddress(String addr) {
            final String target = addr;
            runOnUiThread(() -> {
                String origin = normalizeAddr(target);
                if (origin == null) {
                    toast("地址无效：填 电脑IP:端口（例 192.168.10.242:45679）");
                    return;
                }
                prefs.edit().putString(KEY_ADDR, origin).apply();
                openWebView(origin, null);
            });
        }

        /**
         * 跳到另一个服务器（网页登录页换了地址，或扫到了别的电脑的码）。
         * 走壳的 connectTo：证书信任与地址记忆都在壳侧，配对链接的指纹随码 pin 也在壳侧做。
         */
        @JavascriptInterface
        public void connectTo(String url) {
            final String target = url;
            runOnUiThread(() -> connectToUrl(target));
        }

        @JavascriptInterface
        public void scanQrCode() {
            runOnUiThread(() -> {
                scanForWeb = true;
                startScan();
            });
        }
    }

    /** 把网页注册的回调 window.__fridayScanCallback 叫起来，把扫描结果交回网页。 */
    private void notifyScanResult(String text) {
        if (webView == null) return;
        String js = "window.__fridayScanCallback && window.__fridayScanCallback("
                + JSONObject.quote(text == null ? "" : text) + ");";
        try {
            webView.evaluateJavascript(js, null);
        } catch (Exception e) {
            // 网页还没注册回调或已卸载：忽略（用户取消扫码是正常路径）
        }
    }

    /* ---------- 杂项 ---------- */

    private void toast(String msg) {
        Toast.makeText(this, msg, Toast.LENGTH_LONG).show();
    }

    private int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }
}
