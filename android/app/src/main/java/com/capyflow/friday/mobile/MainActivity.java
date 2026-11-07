package com.capyflow.friday.mobile;

import android.annotation.SuppressLint;
import android.net.http.SslCertificate;
import android.net.http.SslError;
import android.os.Bundle;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.SslErrorHandler;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import androidx.activity.OnBackPressedCallback;
import androidx.appcompat.app.AlertDialog;
import androidx.appcompat.app.AppCompatActivity;

import com.journeyapps.barcodescanner.ScanContract;
import com.journeyapps.barcodescanner.ScanOptions;

import java.io.ByteArrayInputStream;
import java.security.MessageDigest;
import java.security.cert.CertificateFactory;
import java.security.cert.X509Certificate;

/**
 * Friday Mobile 壳：极薄 WebView，加载内核的移动端 PWA（/mobile）。
 * 壳只负责两件浏览器做不到的事——记住服务端地址、TOFU 信任自签证书；
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

    private android.content.SharedPreferences prefs;

    private FrameLayout root;
    private LinearLayout connectView;
    private EditText addrInput;
    private WebView webView;
    private boolean webReady = false;
    private String currentOrigin = null;

    private final androidx.activity.result.ActivityResultLauncher<ScanOptions> scanLauncher =
            registerForActivityResult(new ScanContract(), result -> {
                if (result.getContents() != null) handleScannedUrl(result.getContents());
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
        buildWebView();

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

    /* ---------- 连接视图 ---------- */

    private void showConnect() {
        webReady = false;
        root.removeAllViews();
        root.addView(connectView);
    }

    private void buildConnectView() {
        connectView = new LinearLayout(this);
        connectView.setOrientation(LinearLayout.VERTICAL);
        connectView.setPadding(dp(28), dp(80), dp(28), dp(28));
        connectView.setGravity(Gravity.CENTER_HORIZONTAL);

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
        scan.setOnClickListener(v -> {
            ScanOptions opt = new ScanOptions()
                    .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                    .setPrompt("扫描电脑端「手机连接」二维码")
                    .setBeepEnabled(false)
                    .setOrientationLocked(true);
            scanLauncher.launch(opt);
        });
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

    private void handleScannedUrl(String raw) {
        android.net.Uri uri = android.net.Uri.parse(raw.trim());
        if (!"https".equals(uri.getScheme()) || uri.getHost() == null
                || !uri.getPath().endsWith("/mobile")) {
            toast("二维码不是 Friday 配对码");
            return;
        }
        String addr = uri.getScheme() + "://" + uri.getAuthority();
        // 扫码即带外可信通道：指纹随码校验，直接 pin，免 TOFU 人工确认
        String fp = uri.getQueryParameter("fp");
        if (fp != null && fp.length() == 64) {
            pinFingerprint(addr, fp);
        }
        prefs.edit().putString(KEY_ADDR, addr).apply();
        openWebView(addr, raw.trim());
    }

    /* ---------- WebView ---------- */

    @SuppressLint("SetJavaScriptEnabled")
    private void buildWebView() {
        webView = new WebView(this);
        webView.getSettings().setJavaScriptEnabled(true);
        webView.getSettings().setDomStorageEnabled(true); // 登录态 localStorage
    }

    private void openWebView(String origin, String fullUrl) {
        currentOrigin = origin;
        if (webView.getParent() != null) root.removeView(webView);
        root.removeAllViews();
        root.addView(webViewHost());
        webReady = false;
        webView.setWebViewClient(client());
        webView.loadUrl(fullUrl != null ? fullUrl : origin + "/mobile");
    }

    /** WebView 外面垫一层容器：加载失败时能看到返回连接页的浮层按钮。 */
    private View webViewHost() {
        FrameLayout host = new FrameLayout(this);
        host.addView(webView, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        Button reset = new Button(this);
        reset.setText("更换服务端地址");
        reset.setAlpha(0f); // 平时隐形；加载失败时置为可见
        reset.setOnClickListener(v -> {
            addrInput.setText(currentOrigin == null ? "" : currentOrigin.replaceFirst("^https://", ""));
            showConnect();
        });
        reset.setTag("reset");
        FrameLayout.LayoutParams lp = new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT,
                Gravity.CENTER);
        host.addView(reset, lp);
        webView.setTag(reset); // client 里按需取
        return host;
    }

    private WebViewClient client() {
        return new WebViewClient() {
            @Override
            public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
                tofuCheck(handler, error.getCertificate());
            }

            @Override
            public void onReceivedError(WebView view, int errorCode, String description, String failingUrl) {
                // 只关心主框架首屏失败（子资源失败不回连接页）
                if (!webReady && failingUrl != null && failingUrl.startsWith(currentOrigin + "/mobile")) {
                    toast("连接失败：检查地址与 Wi-Fi 网络");
                    Button reset = webView.getTag() instanceof Button ? (Button) webView.getTag() : null;
                    if (reset != null) {
                        reset.setAlpha(1f);
                        reset.bringToFront();
                    }
                }
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                webReady = true;
                View reset = webView.getTag() instanceof View ? (View) webView.getTag() : null;
                if (reset != null) reset.setAlpha(0f);
            }
        };
    }

    /* ---------- 证书 TOFU ---------- */

    private void tofuCheck(SslErrorHandler handler, SslCertificate cert) {
        String fp = certificateFingerprint(cert);
        String host = currentOrigin == null ? "" : currentOrigin.replaceFirst("^https://", "");
        String pinned = prefs.getString(KEY_FP_PREFIX + host, null);
        if (fp != null && fp.equals(pinned)) {
            handler.proceed();
            return;
        }
        if (fp == null) { // 证书取不出来（极罕见），宁可拒绝也不放行
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

    private void pinFingerprint(String origin, String fp) {
        String host = origin.replaceFirst("^https://", "");
        prefs.edit().putString(KEY_FP_PREFIX + host, fp).apply();
    }

    private String certificateFingerprint(SslCertificate cert) {
        try {
            android.os.Bundle state = SslCertificate.saveState(cert);
            byte[] der = state == null ? null : state.getByteArray("x509");
            if (der == null) return null;
            X509Certificate x = (X509Certificate) CertificateFactory.getInstance("X.509")
                    .generateCertificate(new ByteArrayInputStream(der));
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] digest = md.digest(x.getEncoded());
            StringBuilder sb = new StringBuilder();
            for (byte by : digest) sb.append(String.format("%02x", by));
            return sb.toString();
        } catch (Exception e) {
            return null;
        }
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

    /* ---------- 杂项 ---------- */

    private void toast(String msg) {
        Toast.makeText(this, msg, Toast.LENGTH_LONG).show();
    }

    private int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }
}
