<script setup>
import { ref, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import auth from '../stores/auth'

const router = useRouter()
const route = useRoute()
const loading = ref(false)
const form = ref({ username: '', password: '' })

onMounted(async () => {
  // 鉴权关闭则直接进入主界面。
  if (!auth.state.initialized) await auth.refresh()
  if (!auth.state.enabled || auth.state.me) {
    if (auth.state.me?.mustChangePassword) router.replace('/change-password')
    else router.replace(route.query.redirect || '/dashboard')
  }
})

async function submit() {
  if (!form.value.username || !form.value.password) {
    ElMessage.warning('请输入用户名和密码')
    return
  }
  loading.value = true
  try {
    const res = await auth.login(form.value.username, form.value.password)
    if (res.mustChangePassword) router.replace('/change-password')
    else router.replace(route.query.redirect || '/dashboard')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <div class="login-card">
      <div class="login-brand">
        <span class="brand-mark">◆</span>
        <span class="brand-name">IoT 网关</span>
      </div>
      <p class="login-sub">请登录以管理网关</p>
      <el-form @submit.prevent="submit">
        <el-form-item>
          <el-input v-model="form.username" placeholder="用户名" size="large" autofocus @keyup.enter="submit" />
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.password"
            type="password"
            placeholder="密码"
            size="large"
            show-password
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" size="large" style="width: 100%" :loading="loading" @click="submit">
          登录
        </el-button>
      </el-form>
      <p class="login-hint">默认账号 admin / admin123,首次登录须修改密码</p>
    </div>
  </div>
</template>

<style scoped>
.login-wrap {
  height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(160deg, #eef2ff 0%, #f6f7f9 60%);
}
.login-card {
  width: 360px;
  background: #fff;
  border: 1px solid #e7e9ee;
  border-radius: 14px;
  box-shadow: 0 8px 30px rgba(16, 24, 40, 0.08);
  padding: 36px 32px 30px;
}
.login-brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 18px;
  font-weight: 650;
}
.login-brand .brand-mark {
  color: #2563eb;
}
.login-sub {
  margin: 8px 0 22px;
  color: #9aa1ac;
  font-size: 13px;
}
.login-hint {
  margin-top: 18px;
  color: #c1c7d0;
  font-size: 12px;
  text-align: center;
}
</style>
