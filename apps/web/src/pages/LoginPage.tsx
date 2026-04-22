import { useState } from "react";
import { useNavigate } from "react-router-dom";

export function LoginPage() {
  const navigate = useNavigate();
  const [tenant, setTenant] = useState("");
  const [token, setToken] = useState("");

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    localStorage.setItem("kapp.tenant", tenant);
    if (token) localStorage.setItem("kapp.token", token);
    navigate("/");
  };

  return (
    <form onSubmit={submit} style={{ maxWidth: 360 }}>
      <h1>Sign in</h1>
      <label>
        Tenant
        <input value={tenant} onChange={(e) => setTenant(e.target.value)} />
      </label>
      <label>
        Token (optional)
        <input value={token} onChange={(e) => setToken(e.target.value)} />
      </label>
      <button type="submit">Continue</button>
    </form>
  );
}
