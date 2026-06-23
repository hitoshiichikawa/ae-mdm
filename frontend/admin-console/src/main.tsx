import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';
import './styles.css';

// admin-console SPA エントリポイント (SuperAdmin 専用)。
// 本 Issue (#1) では scaffold のみ。テナント管理・横断ダッシュボード等は umbrella task 13.x で追加。
const rootElement = document.getElementById('root');
if (!rootElement) {
  throw new Error('Root element #root not found in index.html');
}

ReactDOM.createRoot(rootElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
