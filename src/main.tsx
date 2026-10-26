import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { AuthGate } from "./components/auth-gate";
import "./index.css";

const Services = React.lazy(() => import("./pages/Services"));

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <AuthGate>
      {window.location.pathname === "/services" ? (
        <React.Suspense
          fallback={
            <div className="p-8 text-muted-foreground">正在加载服务监控…</div>
          }
        >
          <Services />
        </React.Suspense>
      ) : (
        <App />
      )}
    </AuthGate>
  </React.StrictMode>,
);
