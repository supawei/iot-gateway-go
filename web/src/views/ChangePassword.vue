<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import auth from '../stores/auth'

const router = useRouter()
const loading = ref(false)
const form = ref({ oldPassword: '', newPassword: '', confirm: '' })

async function submit() {
  if (!form.value.oldPassword || !form.value.newPassword) {
    ElMessage.warning('请填写旧密码和新密码')
    return
  }
  if (form.value.newPassword !== form.value.confirm) {
    ElMessage.warning('两次输入的新密码不一致')
    return
  }
  loading.value = true
  try {
    await auth.changePassword(form.value.oldPassword, form.value.newPassword)
    ElMessage.success('密码已修改')
    router.replace('/dashboard')
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
        <span class="brand-name">修改密码</span>
      </div>
      <p class="login-sub">首次登录须修改默认密码后才能使用</p>
      <el-form @submit.prevent="submit">
        <el-form-item>
          <el-input v-model="form.oldPassword" type="password" placeholder="旧密码" size="large" show-password />
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.newPassword"
            type="password"
            placeholder="新密码"
            size="large"
            show-password
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.confirm"
            type="password"
            placeholder="确认新密码"
            size="large"
            show-password
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" size="large" style="width: 100%" :loading="loading" @click="submit">
          确认修改
        </el-button>
      </el-form>
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
  width: 380px;
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
</style>
