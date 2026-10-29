import React from "react";
import ReactDOM from "react-dom/client";
import { AuthGate } from "./components/auth-gate";
import "./index.css";

const Services = React.lazy(() => import("./pages/Services"));
const App = React.lazy(() => import("./App"));

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <AuthGate>
      <React.Suspense
        fallback={
          <div className="p-8 text-muted-foreground" role="status">
            正在加载页面…
          </div>
        }
      >
        {window.location.pathname === "/services" ? <Services /> : <App />}
      </React.Suspense>
    </AuthGate>
  </React.StrictMode>,
);
