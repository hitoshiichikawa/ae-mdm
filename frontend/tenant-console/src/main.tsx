import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';
import './styles.css';

// tenant-console SPA エントリポイント。
// 本 Issue (#1) では scaffold のみ。OIDC ログイン・ルーティング・ドメイン UI は
// umbrella task 12.1〜12.4 で追加する。
const rootElement = document.getElementById('root');
if (!rootElement) {
  throw new Error('Root element #root not found in index.html');
}

ReactDOM.createRoot(rootElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
