/**
 * AI Gateway Cloudflare Worker Reverse Proxy Relay
 * 
 * High-performance edge proxy for routing OpenAI, Anthropic, Gemini, Groq, 
 * Mistral, DeepSeek, and other LLM APIs through Cloudflare's global edge network.
 * 
 * Features:
 * - 100% transparent streaming (SSE) with zero buffering
 * - IP masking (bypasses geographical blocks & ISP filtering)
 * - Optional authentication via X-Relay-Secret header or ?secret= query param
 * - Target override via X-Target-URL header or path-based routing (/openai, /anthropic, etc.)
 * - Zero cold-start, free tier includes 100,000 requests/day
 * 
 * How to deploy:
 * 1. Log in to dash.cloudflare.com -> Workers & Pages -> Create Worker
 * 2. Paste this entire script into the code editor.
 * 3. (Optional) Set an environment variable RELAY_SECRET in Worker Settings -> Variables.
 * 4. Click "Deploy". Your worker URL will be: https://<worker-name>.<subdomain>.workers.dev
 * 5. In your AI Gateway config.yaml or Web Dashboard, set:
 *    relay_url: https://<worker-name>.<subdomain>.workers.dev
 *    relay_secret: <your-secret-if-set>
 */

// Default upstream mappings by path prefix
const DEFAULT_TARGETS = {
  "openai": "https://api.openai.com",
  "anthropic": "https://api.anthropic.com",
  "gemini": "https://generativelanguage.googleapis.com",
  "groq": "https://api.groq.com/openai",
  "mistral": "https://api.mistral.ai",
  "deepseek": "https://api.deepseek.com",
  "opencode": "https://opencode.net/api",
  "default": "https://api.openai.com"
};

export default {
  async fetch(request, env, ctx) {
    // 1. Handle CORS Preflight
    if (request.method === "OPTIONS") {
      return new Response(null, {
        status: 204,
        headers: {
          "Access-Control-Allow-Origin": "*",
          "Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, OPTIONS",
          "Access-Control-Allow-Headers": "Content-Type, Authorization, X-Relay-Secret, X-Target-URL, anthropic-version",
          "Access-Control-Max-Age": "86400"
        }
      });
    }

    // 2. Authenticate Relay Secret (if configured in Worker env)
    const requiredSecret = env.RELAY_SECRET;
    if (requiredSecret) {
      const url = new URL(request.url);
      const providedSecret = request.headers.get("X-Relay-Secret") || url.searchParams.get("secret");
      if (!providedSecret || providedSecret !== requiredSecret) {
        return new Response(JSON.stringify({
          error: {
            message: "Unauthorized: Invalid or missing X-Relay-Secret header",
            type: "relay_auth_error"
          }
        }), {
          status: 401,
          headers: { "Content-Type": "application/json" }
        });
      }
    }

    const url = new URL(request.url);

    // 3. Resolve Target Upstream Base URL
    // Priority:
    // a. X-Target-URL header
    // b. Path prefix routing: /openai/v1/... -> https://api.openai.com/v1/...
    // c. Default fallback: https://api.openai.com
    let targetBaseURL = request.headers.get("X-Target-URL");
    let targetPath = url.pathname;

    if (!targetBaseURL) {
      const pathSegments = url.pathname.split("/").filter(Boolean);
      if (pathSegments.length > 0 && DEFAULT_TARGETS[pathSegments[0].toLowerCase()]) {
        const prefix = pathSegments[0].toLowerCase();
        targetBaseURL = DEFAULT_TARGETS[prefix];
        targetPath = "/" + pathSegments.slice(1).join("/");
      } else {
        targetBaseURL = DEFAULT_TARGETS["default"];
      }
    }

    // Assemble final target URL
    const targetURL = new URL(targetPath + url.search, targetBaseURL);

    // 4. Prepare Outbound Headers (Clean CF-internal tracing headers)
    const newHeaders = new Headers(request.headers);
    newHeaders.delete("X-Relay-Secret");
    newHeaders.delete("X-Target-URL");
    newHeaders.delete("cf-connecting-ip");
    newHeaders.delete("cf-ipcountry");
    newHeaders.delete("cf-ray");
    newHeaders.delete("cf-visitor");
    newHeaders.delete("x-forwarded-proto");
    newHeaders.delete("x-real-ip");
    
    // Set appropriate host
    newHeaders.set("Host", targetURL.host);

    // 5. Fetch from Upstream with Full Streaming
    try {
      const upstreamResponse = await fetch(targetURL.toString(), {
        method: request.method,
        headers: newHeaders,
        body: ["GET", "HEAD"].includes(request.method) ? undefined : request.body,
        redirect: "follow"
      });

      // Prepare Response Headers
      const responseHeaders = new Headers(upstreamResponse.headers);
      responseHeaders.set("Access-Control-Allow-Origin", "*");
      responseHeaders.set("X-Relayed-By", "AI-Gateway-Edge-Worker");

      return new Response(upstreamResponse.body, {
        status: upstreamResponse.status,
        statusText: upstreamResponse.statusText,
        headers: responseHeaders
      });
    } catch (err) {
      return new Response(JSON.stringify({
        error: {
          message: "Relay upstream connection failed: " + err.message,
          type: "relay_upstream_error"
        }
      }), {
        status: 502,
        headers: {
          "Content-Type": "application/json",
          "Access-Control-Allow-Origin": "*"
        }
      });
    }
  }
};
